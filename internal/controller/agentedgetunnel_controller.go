package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
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
	FinalizerName = "edge.steadyinfra.com/tunnel-cleanup"

	// maxRequeue bounds how long an Active tunnel waits between checks.
	maxRequeue = 30 * time.Second

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "ai-agent-edge-tunnel"

	// Keys in the connector Secret, mounted at connectorConfigDir.
	credentialsKey     = "credentials.json"
	configKey          = "config.yaml"
	connectorConfigDir = "/etc/cloudflared"

	// cloudflared serves /ready (200 once connected to Cloudflare's edge) on
	// its metrics address; the pod's readiness follows it.
	connectorMetricsPort = 2000

	// The official cloudflared image runs as this non-root user.
	connectorUID int64 = 65532
)

// AgentEdgeTunnelReconciler reconciles an AgentEdgeTunnel object
type AgentEdgeTunnelReconciler struct {
	client.Client
	// APIReader bypasses the cache for quota reservation compare-and-swap retries.
	APIReader client.Reader
	Scheme           *runtime.Scheme
	CloudflareClient cf.TunnelService
	// AccountID is the Cloudflare account used for Access applications.
	AccountID string
	// ZoneID pins every AgentEdgeTunnel to the single Cloudflare zone configured
	// for this operator instance. Per-object zone IDs are requests, not authority.
	ZoneID string
	// PublicNamespaces is the explicit allowlist for unauthenticated tunnels.
	// An empty map denies public tunnels in every namespace.
	PublicNamespaces map[string]struct{}
	// BaseDomain is the domain under which hostnames are created, e.g.
	// "agents.example.com". Required.
	BaseDomain string
	// ConnectorImage is the cloudflared image. Pin a version or digest.
	ConnectorImage string
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
//
// +kubebuilder:rbac:groups=edge.steadyinfra.com,resources=agentedgetunnels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=edge.steadyinfra.com,resources=namespacetunnelquotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=edge.steadyinfra.com,resources=namespacetunnelquotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=edge.steadyinfra.com,resources=agentedgetunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=edge.steadyinfra.com,resources=agentedgetunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentsessions,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;delete
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
	orig := tunnel.DeepCopy()
	if controllerutil.AddFinalizer(&tunnel, FinalizerName) {
		if err := r.patchFinalizers(ctx, &tunnel, orig); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Account for already-provisioned tunnels before enforcing a newly-created
	// namespace quota. Existing tunnels are grandfathered if the limit is lower.
	if tunnel.Status.TunnelID != "" {
		if err := r.ensureQuotaReservation(ctx, &tunnel, true); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Existing Cloudflare resources must be checked against current operator
	// policy before reading their AgentSession. A revoked public namespace or
	// changed zone takes precedence over session state and triggers cleanup.
	if tunnel.Status.TunnelID != "" {
		if err := r.validateOperatorPolicy(&tunnel); err != nil {
			return ctrl.Result{}, r.terminate(ctx, &tunnel, "operator security policy changed; resources removed: "+err.Error())
		}
	}

	if res, done, err := r.reconcileSession(ctx, &tunnel); done {
		return res, err
	}

	// Keep the policy check for objects without recorded tunnel IDs, before
	// provisioning can create any Cloudflare resources.
	if err := r.validateOperatorPolicy(&tunnel); err != nil {
		tunnel.Status.Phase = agentopsv1alpha1.PhaseFailed
		tunnel.Status.Message = err.Error()
		return ctrl.Result{}, r.Status().Update(ctx, &tunnel)
	}

	if tunnel.Status.Phase == agentopsv1alpha1.PhaseActive {
		return r.reconcileActive(ctx, &tunnel)
	}
	return r.provision(ctx, &tunnel)
}

// recordedIDs returns the Cloudflare IDs recorded in t's status.
func recordedIDs(t *agentopsv1alpha1.AgentEdgeTunnel) []string {
	var ids []string
	for _, id := range []string{t.Status.TunnelID, t.Status.AccessAppID} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
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
	if err := r.releaseQuotaReservation(ctx, t); err != nil {
		return ctrl.Result{}, fmt.Errorf("releasing namespace quota reservation: %w", err)
	}
	orig := t.DeepCopy()
	controllerutil.RemoveFinalizer(t, FinalizerName)
	return ctrl.Result{}, r.patchFinalizers(ctx, t, orig)
}

// patchFinalizers writes only the finalizer change. A typed Update would send
// the whole object back, and because of omitempty that silently drops
// explicitly set false or empty spec fields (security.allowPublic: false, for
// example). The CRD's immutability rules would then reject the write and the
// object could never be deleted. The patch carries the resourceVersion, so a
// concurrent change still conflicts and is retried.
func (r *AgentEdgeTunnelReconciler) patchFinalizers(ctx context.Context, t, orig *agentopsv1alpha1.AgentEdgeTunnel) error {
	return r.Patch(ctx, t, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// reconcileActive handles an Active object: it cleans up once the TTL has
// passed and otherwise requeues for the expiry.
func (r *AgentEdgeTunnelReconciler) reconcileActive(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (ctrl.Result, error) {
	now := r.now()
	if t.Status.ExpiresAt != nil && !now.Before(t.Status.ExpiresAt.Time) {
		log.FromContext(ctx).Info("AgentEdgeTunnel reached TTL; removing Cloudflare resources", "name", t.Name)
		return ctrl.Result{}, r.terminate(ctx, t, "TTL expired; Cloudflare resources removed")
	}

	// An object that became Active without an Access application (for example
	// under an older operator) is already reachable. Create the policy now and
	// say so loudly until it exists.
	if err := r.ensureAccess(ctx, t); err != nil {
		t.Status.Message = "UNPROTECTED: no Access policy yet: " + err.Error()
		if uerr := r.Status().Update(ctx, t); uerr != nil {
			log.FromContext(ctx).Error(uerr, "Failed to record missing Access policy in status")
		}
		return ctrl.Result{}, fmt.Errorf("protecting active tunnel: %w", err)
	}

	// Keep the connector in its desired state and reflect whether it is
	// connected. A failure is surfaced in status as well as retried.
	ready, err := r.ensureConnector(ctx, t)
	if err != nil {
		t.Status.ConnectorReady = false
		t.Status.Message = err.Error()
		if uerr := r.Status().Update(ctx, t); uerr != nil {
			log.FromContext(ctx).Error(uerr, "Failed to record connector error in status")
		}
		return ctrl.Result{}, fmt.Errorf("ensuring connector: %w", err)
	}
	if msg := statusMessage(t, ready); ready != t.Status.ConnectorReady || msg != t.Status.Message {
		t.Status.ConnectorReady = ready
		t.Status.Message = msg
		if err := r.Status().Update(ctx, t); err != nil {
			return ctrl.Result{}, err
		}
	}

	wait := maxRequeue
	if t.Status.ExpiresAt != nil {
		if until := t.Status.ExpiresAt.Sub(now); until < wait {
			wait = until
		}
	}
	if t.Spec.SessionRef != nil && wait > sessionPollInterval {
		wait = sessionPollInterval
	}
	if wait < time.Second {
		wait = time.Second
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

// terminate removes the Cloudflare resources and the connector, then marks the
// object Terminated, which is final. It is marked only after cleanup succeeded;
// otherwise the object keeps its phase and the caller retries.
func (r *AgentEdgeTunnelReconciler) terminate(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, message string) error {
	if err := r.cleanup(ctx, t); err != nil {
		return fmt.Errorf("cleaning up: %w", err)
	}
	if err := r.releaseQuotaReservation(ctx, t); err != nil { return fmt.Errorf("releasing namespace quota reservation: %w", err) }
	t.Status.Phase = agentopsv1alpha1.PhaseTerminated
	t.Status.Message = message
	t.Status.TunnelID = ""
	t.Status.AccessAppID = ""
	t.Status.ConnectorReady = false
	return r.Status().Update(ctx, t)
}

// provision drives Pending/Failed objects to Active. Every step is safe to
// repeat, and the order matters:
//
//  1. tunnel + credentials (IDs recorded immediately, so retries never leak),
//  2. Access policy, so the hostname is protected before it exists,
//  3. DNS, which is what makes the hostname reachable,
//  4. the cloudflared connector.
//
// If the Access policy cannot be created, nothing after it runs: the hostname
// never goes live unprotected.
func (r *AgentEdgeTunnelReconciler) provision(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ttl, err := validate(t)
	if err != nil {
		// A spec error cannot be fixed by retrying; wait for the user to edit it.
		t.Status.Phase = agentopsv1alpha1.PhaseFailed
		t.Status.Message = err.Error()
		return ctrl.Result{}, r.Status().Update(ctx, t)
	}

	if err := r.ensureQuotaReservation(ctx, t, t.Status.TunnelID != ""); err != nil {
		return ctrl.Result{}, err
	}

	if t.Status.TunnelID == "" {
		// A Secret we cannot use would fail every attempt after the tunnel was
		// already created, so check before making any Cloudflare call.
		if err := r.checkConnectorSecretFree(ctx, t); err != nil {
			return r.fail(ctx, t, err)
		}
		// A tunnel from an earlier attempt that stopped before its ID was
		// recorded would otherwise be orphaned and then duplicated.
		recovered, err := r.recoverTunnel(ctx, t)
		if err != nil {
			return r.fail(ctx, t, err)
		}
		if recovered != nil {
			t.Status.TunnelID = recovered.ID
			t.Status.Hostname = r.hostname(t)
			t.Status.Phase = agentopsv1alpha1.PhasePending
			t.Status.Message = "recovered the tunnel from an earlier attempt; configuring access and DNS"
			logger.Info("Adopted a tunnel from an earlier attempt", "tunnelID", recovered.ID, "tunnelName", recovered.Name)
			if err := r.Status().Update(ctx, t); err != nil {
				// Nothing was created, so a lost write only repeats this lookup.
				return ctrl.Result{}, err
			}
		} else {
			logger.Info("Creating Cloudflare tunnel", "name", t.Name)
			info, err := r.CloudflareClient.CreateTunnel(ctx, tunnelName(t))
			if err != nil {
				return r.fail(ctx, t, fmt.Errorf("creating tunnel: %w", err))
			}
			hostname := r.hostname(t)
			// Cloudflare returns the tunnel secret only once, so it is stored
			// before anything else can fail.
			if err := r.writeConnectorSecret(ctx, t, info, hostname); err != nil {
				r.discardTunnel(ctx, info.ID)
				return r.fail(ctx, t, fmt.Errorf("storing connector credentials: %w", err))
			}
			t.Status.TunnelID = info.ID
			t.Status.Hostname = hostname
			t.Status.Phase = agentopsv1alpha1.PhasePending
			t.Status.Message = "tunnel created; configuring access and DNS"
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
	}

	if err := r.ensureAccess(ctx, t); err != nil {
		return r.fail(ctx, t, err)
	}

	if err := r.CloudflareClient.CreateDNSRecord(ctx, t.Spec.Cloudflare.ZoneID, t.Status.Hostname, t.Status.TunnelID); err != nil {
		return r.fail(ctx, t, fmt.Errorf("creating DNS record: %w", err))
	}

	ready, err := r.ensureConnector(ctx, t)
	if err != nil {
		return r.fail(ctx, t, fmt.Errorf("running connector: %w", err))
	}

	if t.Status.ExpiresAt == nil && ttl > 0 {
		exp := metav1.NewTime(r.now().Add(ttl))
		t.Status.ExpiresAt = &exp
	}
	t.Status.Phase = agentopsv1alpha1.PhaseActive
	t.Status.ConnectorReady = ready
	t.Status.Message = statusMessage(t, ready)
	if err := r.Status().Update(ctx, t); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("AgentEdgeTunnel is active", "hostname", t.Status.Hostname, "connectorReady", ready)
	if t.Spec.SessionRef != nil {
		return ctrl.Result{RequeueAfter: sessionPollInterval}, nil
	}
	return ctrl.Result{RequeueAfter: maxRequeue}, nil
}

// fail records a transient error in status and returns it so the controller
// retries with backoff. IDs already recorded stay recorded.
func (r *AgentEdgeTunnelReconciler) fail(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, cause error) (ctrl.Result, error) {
	t.Status.Phase = agentopsv1alpha1.PhaseFailed
	t.Status.Message = cause.Error()
	if err := r.Status().Update(ctx, t); err != nil {
		log.FromContext(ctx).Error(err, "Failed to record failure in status")
	}
	return ctrl.Result{}, cause
}

// checkConnectorSecretFree fails fast when the connector Secret name is taken
// by an object this tunnel does not own. writeConnectorSecret checks again,
// which covers a Secret created in between.
func (r *AgentEdgeTunnelReconciler) checkConnectorSecretFree(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	if !hasConnector(t) {
		return nil
	}
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: connectorName(t)}, &s); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&s, t) {
		return fmt.Errorf("secret %q already exists and is not managed by this AgentEdgeTunnel; refusing to overwrite it", s.Name)
	}
	return nil
}

// namesAreOwned reports whether t has enough UID data to construct its
// deterministic Cloudflare resource name. The truncated UID suffix is only a
// lookup filter, not proof of ownership; recoverTunnel and recoverAccessApp
// must still require independent evidence before adopting or deleting matches.
func namesAreOwned(t *agentopsv1alpha1.AgentEdgeTunnel) bool {
	return len(t.UID) >= 8
}

// recoverTunnel looks for a tunnel that an earlier attempt created for t but
// never recorded in status. A matching name is not proof of ownership. Recovery
// is allowed only when the connector Secret is controlled by t and contains
// credentials naming the candidate tunnel. If that evidence is absent or
// mismatched, recovery stops and requires operator resolution; it never deletes
// a same-name tunnel based on a failed ownership check.
func (r *AgentEdgeTunnelReconciler) recoverTunnel(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (*cf.TunnelRef, error) {
	if !namesAreOwned(t) {
		return nil, nil
	}
	found, err := r.CloudflareClient.FindTunnels(ctx, tunnelName(t))
	if err != nil {
		return nil, fmt.Errorf("looking for a tunnel from an earlier attempt: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("found %d Cloudflare tunnels named %q; ownership is ambiguous and requires operator resolution", len(found), tunnelName(t))
	}
	ref := found[0]
	ok, err := r.credentialsMatch(ctx, t, ref.ID)
	if err != nil {
		return nil, fmt.Errorf("verifying ownership of Cloudflare tunnel %s: %w", ref.ID, err)
	}
	if !ok {
		return nil, fmt.Errorf("cloudflare tunnel %q (%s) matches this AgentEdgeTunnel's name, but ownership cannot be established; leave it untouched and resolve it manually", ref.Name, ref.ID)
	}
	return &ref, nil
}

// credentialsMatch reports whether the connector Secret controlled by t contains
// credentials for tunnelID. Without a connector Secret there is no independent
// ownership evidence, so recovery must not adopt or delete a same-name tunnel.
func (r *AgentEdgeTunnelReconciler) credentialsMatch(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, tunnelID string) (bool, error) {
	if !hasConnector(t) {
		return false, nil
	}
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: connectorName(t)}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !metav1.IsControlledBy(&s, t) {
		return false, nil
	}
	var creds struct {
		AccountTag   string
		TunnelSecret string
		TunnelID     string
	}
	if err := json.Unmarshal(s.Data[credentialsKey], &creds); err != nil {
		// Malformed credentials provide no ownership evidence; this is not a reconcile error.
		//nolint:nilerr // Deliberately treat malformed credentials as an ownership mismatch.
		return false, nil
	}
	secret, err := base64.StdEncoding.DecodeString(creds.TunnelSecret)
	if err != nil || len(secret) != 32 {
		// Invalid tunnel secrets are absent ownership evidence, not a reconcile error.
		//nolint:nilerr // Deliberately treat invalid secrets as an ownership mismatch.
		return false, nil
	}
	return creds.AccountTag == r.AccountID && creds.TunnelID == tunnelID, nil
}

// discardTunnel removes a tunnel whose ID could not be recorded.
func (r *AgentEdgeTunnelReconciler) discardTunnel(ctx context.Context, tunnelID string) {
	if err := r.CloudflareClient.DeleteTunnel(ctx, tunnelID); err != nil {
		log.FromContext(ctx).Error(err, "Failed to remove an unrecorded tunnel; it may be orphaned", "tunnelID", tunnelID)
	}
}

// ensureAccess creates the Access application and policy once. Its ID is
// recorded immediately so cleanup can find it.
func (r *AgentEdgeTunnelReconciler) ensureAccess(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	emails := t.Spec.Security.AllowedEmails
	if len(emails) == 0 || t.Status.AccessAppID != "" {
		return nil
	}
	appID, err := r.recoverAccessApp(ctx, t)
	if err != nil {
		return err
	}
	if appID == "" {
		appID, err = r.CloudflareClient.CreateAccessPolicy(ctx, r.AccountID, tunnelName(t), t.Status.Hostname, emails)
		if err != nil {
			return fmt.Errorf("creating Access policy: %w", err)
		}
	}
	t.Status.AccessAppID = appID
	if err := r.Status().Update(ctx, t); err != nil {
		// Same reasoning as for the tunnel ID in provision: the error does not
		// prove the write was rejected. Deleting the application could leave
		// status naming one that no longer exists, so the hostname would later
		// go live with no policy. Keep it, and log the ID in case it was lost.
		log.FromContext(ctx).Error(err, "Status write failed after the Access application was created; keeping it because the write may still commit. If it was lost, delete the application by hand", "appID", appID)
		return err
	}
	return nil
}

// recoverAccessApp looks for an Access application that an earlier attempt
// may have created for t but never recorded. Cloudflare's application lookup
// exposes name and domain, not an owner identity; neither is sufficient proof
// that this object created the application. Therefore every match is left
// untouched and recovery stops for operator resolution. This deliberately
// avoids adopting a pre-existing application or deleting an active one.
func (r *AgentEdgeTunnelReconciler) recoverAccessApp(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (string, error) {
	if !namesAreOwned(t) {
		return "", nil
	}
	found, err := r.CloudflareClient.FindAccessApplications(ctx, r.AccountID, tunnelName(t), t.Status.Hostname)
	if err != nil {
		return "", fmt.Errorf("looking for an Access application from an earlier attempt: %w", err)
	}
	if len(found) > 0 {
		ids := make([]string, 0, len(found))
		for _, app := range found {
			ids = append(ids, app.ID)
		}
		return "", fmt.Errorf("found matching Cloudflare Access application(s) %s for %q, but ownership and policy identity cannot be established; leave them untouched and resolve them manually", strings.Join(ids, ", "), t.Status.Hostname)
	}
	return "", nil
}

// cleanup removes everything the tunnel created. The connector goes first,
// because Cloudflare will not delete a tunnel that still has live connections.
// DNS cleanup is independent, but the Access application is retained until
// the tunnel has been deleted successfully. A failed connector removal skips
// tunnel and Access deletion. All failures are returned together so the finalizer stays
// until everything is gone, and the IDs stay in status so a retry finds what is
// left. Every step is idempotent.
func (r *AgentEdgeTunnelReconciler) cleanup(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	if t.Status.TunnelID == "" {
		return nil
	}
	if err := r.deleteConnector(ctx, t); err != nil {
		// Without this the tunnel cannot be deleted. DNS can still be removed,
		// but the Access application is retained while the tunnel remains.
		connectorErr := fmt.Errorf("removing connector: %w", err)
		return errors.Join(connectorErr, r.cleanupCloudflare(ctx, t, false))
	}
	return r.cleanupCloudflare(ctx, t, true)
}

// cleanupCloudflare attempts DNS cleanup and tunnel deletion independently.
// The Access application is deleted only after tunnel deletion succeeds; when
// connector removal failed and deleteTunnel is false, the application is kept.
func (r *AgentEdgeTunnelReconciler) cleanupCloudflare(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, deleteTunnel bool) error {
	var errs []error
	if t.Status.Hostname != "" {
		if err := r.CloudflareClient.DeleteDNSRecord(ctx, t.Spec.Cloudflare.ZoneID, t.Status.Hostname, t.Status.TunnelID); err != nil {
			errs = append(errs, fmt.Errorf("deleting DNS record: %w", err))
		}
	}
	tunnelDeleted := false
	if deleteTunnel {
		if err := r.CloudflareClient.DeleteTunnel(ctx, t.Status.TunnelID); err != nil {
			errs = append(errs, fmt.Errorf("deleting tunnel: %w", err))
		} else {
			tunnelDeleted = true
		}
	}
	if deleteTunnel && tunnelDeleted && t.Status.AccessAppID != "" {
		if err := r.CloudflareClient.DeleteAccessApplication(ctx, r.AccountID, t.Status.AccessAppID); err != nil {
			errs = append(errs, fmt.Errorf("deleting Access application: %w", err))
		}
	}
	return errors.Join(errs...)
}

// --- connector workload -----------------------------------------------------

// connectorName is the name shared by the connector Deployment and its Secret.
func connectorName(t *agentopsv1alpha1.AgentEdgeTunnel) string {
	return t.Name + "-cloudflared"
}

// connectorLabels are the labels on, and the selector of, the connector pods.
func connectorLabels(t *agentopsv1alpha1.AgentEdgeTunnel) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "cloudflared",
		"app.kubernetes.io/instance": t.Name,
		managedByLabel:               managedByValue,
	}
}

// connectorConfig renders cloudflared's config.yaml. Everything interpolated
// is validated: the service name and port by the CRD and by validate, the
// hostname by the CRD pattern plus the operator's base domain.
func connectorConfig(tunnelID, hostname string, svc *agentopsv1alpha1.ServiceTarget, namespace string) string {
	return fmt.Sprintf(`tunnel: %s
credentials-file: %s/%s
ingress:
  - hostname: %s
    service: http://%s.%s.svc:%d
  - service: http_status:404
`, tunnelID, connectorConfigDir, credentialsKey, hostname, svc.Name, namespace, svc.Port)
}

// writeConnectorSecret stores the tunnel credentials and cloudflared config.
// It overwrites a Secret this tunnel already owns (left by an earlier failed
// attempt) and refuses to touch one it does not own.
func (r *AgentEdgeTunnelReconciler) writeConnectorSecret(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, info *cf.TunnelInfo, hostname string) error {
	if !hasConnector(t) {
		return nil
	}
	svc, err := target(t)
	if err != nil {
		return err
	}
	creds, err := json.Marshal(struct {
		AccountTag   string
		TunnelSecret string
		TunnelID     string
	}{info.AccountID, info.Secret, info.ID})
	if err != nil {
		return err
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: connectorName(t), Namespace: t.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, t) {
			return fmt.Errorf("secret %q already exists and is not managed by this AgentEdgeTunnel; refusing to overwrite it", secret.Name)
		}
		secret.Labels = connectorLabels(t)
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			credentialsKey: creds,
			configKey:      []byte(connectorConfig(info.ID, hostname, svc, t.Namespace)),
		}
		return controllerutil.SetControllerReference(t, secret, r.Scheme)
	})
	return err
}

// ensureConnector keeps the cloudflared Deployment in its desired state and
// reports whether it is connected. With no spec.service there is nothing to run.
func (r *AgentEdgeTunnelReconciler) ensureConnector(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (bool, error) {
	if !hasConnector(t) {
		return false, nil
	}

	// Cloudflare only returns the tunnel credentials at creation, so a missing
	// Secret cannot be regenerated.
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: connectorName(t)}, &secret); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, fmt.Errorf("connector credentials Secret %q is missing; Cloudflare returns them only once, so delete and recreate this AgentEdgeTunnel", connectorName(t))
		}
		return false, err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: connectorName(t), Namespace: t.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		return r.mutateConnector(dep, t)
	}); err != nil {
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

// mutateConnector sets the desired state on dep in place.
func (r *AgentEdgeTunnelReconciler) mutateConnector(dep *appsv1.Deployment, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	if dep.ResourceVersion != "" && !metav1.IsControlledBy(dep, t) {
		return fmt.Errorf("deployment %q already exists and is not managed by this AgentEdgeTunnel; refusing to adopt it", dep.Name)
	}

	labels := connectorLabels(t)
	dep.Labels = labels
	if dep.Spec.Selector == nil { // immutable after creation
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}
	// One connector per tunnel. Running two would add redundancy at the cost of
	// a second set of edge connections; make it a field if that is needed.
	dep.Spec.Replicas = int32Ptr(1)

	dep.Spec.Template.Labels = labels
	ps := &dep.Spec.Template.Spec
	// cloudflared never talks to the Kubernetes API.
	ps.AutomountServiceAccountToken = boolPtr(false)
	ps.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(connectorUID),
		RunAsGroup:     int64Ptr(connectorUID),
		FSGroup:        int64Ptr(connectorUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	ps.Volumes = []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  connectorName(t),
			DefaultMode: int32Ptr(0o440),
		}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	// Defaults the API server would otherwise fill in are set explicitly. The
	// container list is replaced wholesale, so leaving them out would make every
	// reconcile look like a change and send a needless update.
	ps.Containers = []corev1.Container{{
		Name:            "cloudflared",
		Image:           r.ConnectorImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			"tunnel", "--no-autoupdate",
			"--metrics", fmt.Sprintf("0.0.0.0:%d", connectorMetricsPort),
			"--config", connectorConfigDir + "/" + configKey,
			"run",
		},
		Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: connectorMetricsPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/ready", Port: intstr.FromInt32(connectorMetricsPort), Scheme: corev1.URISchemeHTTP,
			}},
			PeriodSeconds:    10,
			TimeoutSeconds:   1,
			SuccessThreshold: 1,
			FailureThreshold: 3,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: connectorConfigDir, ReadOnly: true},
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}}
	return controllerutil.SetControllerReference(t, dep, r.Scheme)
}

// deleteConnector removes the connector Deployment and Secret, but only if
// this tunnel owns them.
func (r *AgentEdgeTunnelReconciler) deleteConnector(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	if !hasConnector(t) {
		return nil
	}
	meta := metav1.ObjectMeta{Name: connectorName(t), Namespace: t.Namespace}
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: meta},
		&corev1.Secret{ObjectMeta: meta},
	} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, t) {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return err
		}
	}
	return nil
}

// statusMessage explains why an Active tunnel is not yet fully usable.
func statusMessage(t *agentopsv1alpha1.AgentEdgeTunnel, connectorReady bool) string {
	switch {
	case !hasConnector(t):
		return "no spec.service or spec.sessionRef: the tunnel exists but is not connected to any workload"
	case !connectorReady:
		return "waiting for the cloudflared connector to connect"
	}
	return ""
}

// --- helpers ----------------------------------------------------------------

// validate checks the spec and returns the parsed TTL. Spec errors are not
// retried. The CRD enforces the same rules; checking here as well covers an
// out-of-date CRD, so an invalid object can never cause a tunnel, an Access
// application or a connector to be created.
func validate(t *agentopsv1alpha1.AgentEdgeTunnel) (time.Duration, error) {
	if t.Spec.Cloudflare.ZoneID == "" || t.Spec.Cloudflare.Subdomain == "" {
		return 0, errors.New("spec.cloudflare.zoneID and spec.cloudflare.subdomain are required")
	}
	// The hostname and the connector config are built from this, so it must be
	// a single DNS label; the CRD checks the same pattern.
	if errs := validation.IsDNS1123Label(t.Spec.Cloudflare.Subdomain); len(errs) > 0 {
		return 0, fmt.Errorf("spec.cloudflare.subdomain must be a single DNS label: %s", strings.Join(errs, "; "))
	}
	ttl, err := parseTTL(t.Spec.TTL)
	if err != nil {
		return 0, err
	}
	// connectorConfig interpolates these into YAML, so they are checked here
	// even though the CRD checks them too.
	if t.Spec.Service != nil && t.Spec.SessionRef != nil {
		return 0, errors.New("spec.service and spec.sessionRef are mutually exclusive")
	}
	if ref := t.Spec.SessionRef; ref != nil {
		if errs := validation.IsDNS1123Subdomain(ref.Name); len(errs) > 0 {
			return 0, fmt.Errorf("spec.sessionRef.name is invalid: %s", strings.Join(errs, "; "))
		}
	}
	if hasConnector(t) && len(t.Name) > validation.LabelValueMaxLength {
		// The name becomes a label value on the connector pods.
		return 0, fmt.Errorf("metadata.name must be at most %d characters when a connector is created", validation.LabelValueMaxLength)
	}
	if svc := t.Spec.Service; svc != nil {
		if errs := validation.IsDNS1035Label(svc.Name); len(errs) > 0 {
			return 0, fmt.Errorf("spec.service.name is invalid: %s", strings.Join(errs, "; "))
		}
		if svc.Port < 1 || svc.Port > 65535 {
			return 0, errors.New("spec.service.port must be between 1 and 65535")
		}
	}
	sec := t.Spec.Security
	// The CRD rejects these too; checking here covers an out-of-date CRD.
	if sec.WebhookValidation != nil || sec.ServiceTokens {
		return 0, errors.New("security.webhookValidation and security.serviceTokens are not implemented yet and would not be enforced")
	}
	// A tunnel is never public by omission.
	if len(sec.AllowedEmails) == 0 && !sec.AllowPublic {
		return 0, errors.New("no access policy: set security.allowedEmails, or security.allowPublic: true to expose the hostname without authentication")
	}
	if len(sec.AllowedEmails) > 0 && sec.AllowPublic {
		return 0, errors.New("security.allowedEmails and security.allowPublic are mutually exclusive")
	}
	return ttl, nil
}

// validateOperatorPolicy applies restrictions that are controlled by the
// operator administrator rather than by an individual AgentEdgeTunnel author.
func (r *AgentEdgeTunnelReconciler) validateOperatorPolicy(t *agentopsv1alpha1.AgentEdgeTunnel) error {
	if r.ZoneID == "" {
		return errors.New("operator Cloudflare zone is not configured; set CLOUDFLARE_ZONE_ID")
	}
	if t.Spec.Cloudflare.ZoneID != r.ZoneID {
		return fmt.Errorf("spec.cloudflare.zoneID does not match the operator's configured Cloudflare zone; this operator is pinned to zone %q", r.ZoneID)
	}
	if t.Spec.Security.AllowPublic {
		if _, allowed := r.PublicNamespaces[t.Namespace]; !allowed {
			return fmt.Errorf("security.allowPublic is not permitted in namespace %q; add the namespace to EDGE_PUBLIC_NAMESPACES to authorize unauthenticated tunnels", t.Namespace)
		}
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

// boolPtr returns a pointer to b.
func boolPtr(b bool) *bool { return &b }

// int32Ptr returns a pointer to i.
func int32Ptr(i int32) *int32 { return &i }

// int64Ptr returns a pointer to i.
func int64Ptr(i int64) *int64 { return &i }

// SetupWithManager registers the reconciler with mgr.
func (r *AgentEdgeTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentopsv1alpha1.AgentEdgeTunnel{}).
		Owns(&appsv1.Deployment{}).
		Watches(&agentopsv1alpha1.NamespaceTunnelQuota{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				var tunnels agentopsv1alpha1.AgentEdgeTunnelList
				if err := r.List(ctx, &tunnels, client.InNamespace(obj.GetNamespace())); err != nil {
					log.FromContext(ctx).Error(err, "Unable to enqueue tunnels after namespace quota change", "namespace", obj.GetNamespace())
					return nil
				}
				requests := make([]reconcile.Request, 0, len(tunnels.Items))
				for i := range tunnels.Items {
					requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tunnels.Items[i])})
				}
				return requests
			},
		)).
		Complete(r)
}
