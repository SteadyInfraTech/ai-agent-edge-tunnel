package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
)

const (
	// FinalizerName blocks deletion of an AgentEdgeTunnel until its Cloudflare
	// resources are gone.
	FinalizerName = "agentops.io/edge-tunnel-cleanup"

	// maxRequeue bounds how long an Active tunnel waits between checks.
	maxRequeue = 30 * time.Second
)

// AgentEdgeTunnelReconciler reconciles an AgentEdgeTunnel object
type AgentEdgeTunnelReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	CloudflareClient cf.TunnelService
	// BaseDomain is the domain under which hostnames are created, e.g.
	// "agents.example.com". Required.
	BaseDomain string
	// DryRun is true when CloudflareClient is the in-memory fake. It keeps
	// dry-run and real state apart: see reconcileMode.
	DryRun bool
	// Now is overridable for tests.
	Now func() time.Time
}

// now returns the current time, or the test clock when one is set.
func (r *AgentEdgeTunnelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile drives one AgentEdgeTunnel towards its desired state: it provisions
// the Cloudflare resources, enforces the TTL, and cleans up on deletion.
func (r *AgentEdgeTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tunnel agentopsv1alpha1.AgentEdgeTunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Before anything acts on the recorded IDs, make sure they belong to the mode
	// this operator is running in.
	if res, done, err := r.reconcileMode(ctx, &tunnel); done {
		return res, err
	}

	if !tunnel.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &tunnel)
	}

	// Terminated is final. Without this guard the provisioning path below would
	// create a brand-new tunnel for an object whose TTL already expired.
	if tunnel.Status.Phase == agentopsv1alpha1.PhaseTerminated {
		return ctrl.Result{}, nil
	}

	// The finalizer must exist before any Cloudflare resource is created, or a
	// delete could race ahead of cleanup and leak the tunnel.
	if controllerutil.AddFinalizer(&tunnel, FinalizerName) {
		if err := r.Update(ctx, &tunnel); err != nil {
			return ctrl.Result{}, err
		}
	}

	if tunnel.Status.Phase == agentopsv1alpha1.PhaseActive {
		return r.reconcileActive(ctx, &tunnel)
	}
	return r.provision(ctx, &tunnel)
}

// recordedIDs returns the Cloudflare IDs recorded in t's status.
func recordedIDs(t *agentopsv1alpha1.AgentEdgeTunnel) []string {
	if t.Status.TunnelID == "" {
		return nil
	}
	return []string{t.Status.TunnelID}
}

// reconcileMode keeps --dry-run and real state from contaminating each other
// on a shared cluster. Fake IDs carry cloudflare.DryRunIDPrefix.
//
//   - A real-mode operator that finds IDs written by dry-run forgets them, so the
//     object is provisioned for real instead of staying "Active" with nothing
//     behind it. It never tries to delete a fake ID.
//   - A dry-run operator that finds IDs of real resources refuses to touch the
//     object. Deleting it would "succeed" against the fake without removing
//     anything, and the finalizer would then be dropped, orphaning the real
//     tunnel. The error keeps the finalizer in place.
//
// done reports that Reconcile must return res and err as they are.
func (r *AgentEdgeTunnelReconciler) reconcileMode(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (res ctrl.Result, done bool, err error) {
	var fakeState, realState bool
	for _, id := range recordedIDs(t) {
		if cf.IsDryRunID(id) {
			fakeState = true
		} else {
			realState = true
		}
	}

	switch {
	case r.DryRun && realState:
		msg := "provisioned against real Cloudflare; dry-run will not modify or delete it (restart without --dry-run)"
		if t.Status.Message != msg && t.DeletionTimestamp.IsZero() {
			t.Status.Message = msg
			if uerr := r.Status().Update(ctx, t); uerr != nil {
				log.FromContext(ctx).Error(uerr, "Failed to record dry-run refusal in status")
			}
		}
		return ctrl.Result{}, true, errors.New(msg)
	case !r.DryRun && fakeState:
		log.FromContext(ctx).Info("Discarding status written by --dry-run", "name", t.Name)
		t.Status = agentopsv1alpha1.AgentEdgeTunnelStatus{}
		if err := r.Status().Update(ctx, t); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{Requeue: true}, true, nil
	}
	return ctrl.Result{}, false, nil
}

// reconcileDelete removes the Cloudflare resources and then releases the
// finalizer. The finalizer is kept whenever cleanup fails.
func (r *AgentEdgeTunnelReconciler) reconcileDelete(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(t, FinalizerName) {
		return ctrl.Result{}, nil
	}
	log.FromContext(ctx).Info("Cleaning up Cloudflare resources for deleted AgentEdgeTunnel", "name", t.Name)
	// On failure the finalizer stays, so the object remains visible and the
	// cleanup is retried with backoff instead of silently leaking.
	if err := r.cleanup(ctx, t); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleaning up Cloudflare resources: %w", err)
	}
	controllerutil.RemoveFinalizer(t, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, t)
}

// reconcileActive handles an Active object: it cleans up once the TTL has
// passed and otherwise requeues for the expiry.
func (r *AgentEdgeTunnelReconciler) reconcileActive(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (ctrl.Result, error) {
	now := r.now()
	if t.Status.ExpiresAt != nil && !now.Before(t.Status.ExpiresAt.Time) {
		log.FromContext(ctx).Info("AgentEdgeTunnel reached TTL; removing Cloudflare resources", "name", t.Name)
		// Only mark Terminated after cleanup succeeded; otherwise stay Active
		// and retry.
		if err := r.cleanup(ctx, t); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up expired tunnel: %w", err)
		}
		t.Status.Phase = agentopsv1alpha1.PhaseTerminated
		t.Status.Message = "TTL expired; Cloudflare resources removed"
		t.Status.TunnelID = ""
		return ctrl.Result{}, r.Status().Update(ctx, t)
	}

	wait := maxRequeue
	if t.Status.ExpiresAt != nil {
		if until := t.Status.ExpiresAt.Sub(now); until < wait {
			wait = until
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

// provision drives Pending/Failed objects to Active. Every step is safe to
// repeat: the tunnel ID is persisted right after creation, and the DNS call is
// idempotent, so a retry never creates a second tunnel.
func (r *AgentEdgeTunnelReconciler) provision(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ttl, err := validate(t)
	if err != nil {
		// A spec error cannot be fixed by retrying; wait for the user to edit it.
		t.Status.Phase = agentopsv1alpha1.PhaseFailed
		t.Status.Message = err.Error()
		return ctrl.Result{}, r.Status().Update(ctx, t)
	}

	if t.Status.TunnelID == "" {
		logger.Info("Creating Cloudflare tunnel", "name", t.Name)
		info, err := r.CloudflareClient.CreateTunnel(ctx, tunnelName(t))
		if err != nil {
			return r.fail(ctx, t, fmt.Errorf("creating tunnel: %w", err))
		}
		t.Status.TunnelID = info.ID
		t.Status.Hostname = r.hostname(t)
		t.Status.Phase = agentopsv1alpha1.PhasePending
		t.Status.Message = "tunnel created; configuring DNS"
		if err := r.Status().Update(ctx, t); err != nil {
			// An error does not prove the write was rejected: a timeout can
			// still have stored the ID. Deleting the tunnel then would leave
			// status pointing at a tunnel that no longer exists, and every
			// retry would reuse that dead ID. A single re-read cannot settle
			// it either, because the write may still commit afterwards. So
			// the tunnel is kept. If the write really was lost, the leak is
			// logged with the tunnel's ID and name, and the retry fails
			// loudly if Cloudflare refuses the duplicate name, instead of
			// silently running against a deleted tunnel.
			logger.Error(err, "Status write failed after the tunnel was created; keeping the tunnel because the write may still commit. If it was lost, delete the tunnel by hand", "tunnelID", info.ID, "tunnelName", tunnelName(t))
			return ctrl.Result{}, err
		}
	}

	if err := r.CloudflareClient.CreateDNSRecord(ctx, t.Spec.Cloudflare.ZoneID, t.Status.Hostname, t.Status.TunnelID); err != nil {
		return r.fail(ctx, t, fmt.Errorf("creating DNS record: %w", err))
	}

	if t.Status.ExpiresAt == nil && ttl > 0 {
		exp := metav1.NewTime(r.now().Add(ttl))
		t.Status.ExpiresAt = &exp
	}
	t.Status.Phase = agentopsv1alpha1.PhaseActive
	t.Status.Message = ""
	if err := r.Status().Update(ctx, t); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("AgentEdgeTunnel is active", "hostname", t.Status.Hostname)
	return ctrl.Result{RequeueAfter: maxRequeue}, nil
}

// fail records a transient error in status and returns it so the controller
// retries with backoff. The tunnel ID, if any, stays recorded.
func (r *AgentEdgeTunnelReconciler) fail(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, cause error) (ctrl.Result, error) {
	t.Status.Phase = agentopsv1alpha1.PhaseFailed
	t.Status.Message = cause.Error()
	if err := r.Status().Update(ctx, t); err != nil {
		log.FromContext(ctx).Error(err, "Failed to record failure in status")
	}
	return ctrl.Result{}, cause
}

// cleanup removes the DNS record, then the tunnel. The tunnel is only deleted
// once DNS is gone, so a retry still has the tunnel ID to work with.
func (r *AgentEdgeTunnelReconciler) cleanup(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	if t.Status.TunnelID == "" {
		return nil
	}
	if t.Status.Hostname != "" {
		if err := r.CloudflareClient.DeleteDNSRecord(ctx, t.Spec.Cloudflare.ZoneID, t.Status.Hostname, t.Status.TunnelID); err != nil {
			return fmt.Errorf("deleting DNS record: %w", err)
		}
	}
	if err := r.CloudflareClient.DeleteTunnel(ctx, t.Status.TunnelID); err != nil {
		return fmt.Errorf("deleting tunnel: %w", err)
	}
	return nil
}

// hostname returns the public hostname for t under the configured base domain.
func (r *AgentEdgeTunnelReconciler) hostname(t *agentopsv1alpha1.AgentEdgeTunnel) string {
	return fmt.Sprintf("%s.%s", t.Spec.Cloudflare.Subdomain, strings.TrimPrefix(r.BaseDomain, "."))
}

// tunnelName is unique per object: names are only unique within a namespace,
// and Cloudflare tunnel names are account-wide.
func tunnelName(t *agentopsv1alpha1.AgentEdgeTunnel) string {
	name := fmt.Sprintf("agent-%s-%s", t.Namespace, t.Name)
	if uid := string(t.UID); len(uid) >= 8 {
		name += "-" + uid[:8]
	}
	return name
}

// validate checks the spec and returns the parsed TTL. Spec errors are not
// retried. The CRD enforces the same rules; checking here as well covers an
// out-of-date CRD, so an invalid object can never cause a tunnel to be created.
func validate(t *agentopsv1alpha1.AgentEdgeTunnel) (time.Duration, error) {
	if t.Spec.Cloudflare.ZoneID == "" || t.Spec.Cloudflare.Subdomain == "" {
		return 0, errors.New("spec.cloudflare.zoneID and spec.cloudflare.subdomain are required")
	}
	return parseTTL(t.Spec.TTL)
}

// parseTTL parses a TTL such as "2h". Empty means no expiry (zero).
func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid ttl %q: %w", s, err)
	}
	if d <= 0 {
		return 0, errors.New("invalid ttl: must be positive")
	}
	return d, nil
}

// SetupWithManager registers the reconciler with mgr.
func (r *AgentEdgeTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentopsv1alpha1.AgentEdgeTunnel{}).
		Complete(r)
}
