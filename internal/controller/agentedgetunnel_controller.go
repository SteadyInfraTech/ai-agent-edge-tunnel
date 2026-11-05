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
	// Now is overridable for tests.
	Now func() time.Time
}

func (r *AgentEdgeTunnelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AgentEdgeTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tunnel agentopsv1alpha1.AgentEdgeTunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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

	ttl, err := parseTTL(t.Spec.TTL)
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
			// If the ID cannot be recorded no later reconcile can clean the
			// tunnel up, so remove it now rather than leak it.
			if delErr := r.CloudflareClient.DeleteTunnel(ctx, info.ID); delErr != nil {
				logger.Error(delErr, "Failed to remove tunnel after status update failure; it may be orphaned", "tunnelID", info.ID)
			}
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

func (r *AgentEdgeTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentopsv1alpha1.AgentEdgeTunnel{}).
		Complete(r)
}
