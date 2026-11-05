package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare/fake"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// harness wires a reconciler to a fake Kubernetes client and a fake Cloudflare.
type harness struct {
	t   *testing.T
	k8s client.Client
	cf  *fake.Client
	r   *AgentEdgeTunnelReconciler
	req ctrl.Request
	now time.Time
}

// withTTL sets the object's ttl.
func withTTL(ttl string) func(*agentopsv1alpha1.AgentEdgeTunnel) {
	return func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.TTL = ttl }
}

// newHarness builds a reconciler around one AgentEdgeTunnel with a one hour ttl;
// mutate, if not nil, adjusts the object before it is stored.
func newHarness(t *testing.T, mutate func(*agentopsv1alpha1.AgentEdgeTunnel)) *harness {
	t.Helper()
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme.Scheme))

	cr := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent-tunnel", Namespace: "default"},
		Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
			TargetPodSelector: map[string]string{"app": "langgraph-agent"},
			Cloudflare:        agentopsv1alpha1.CloudflareConfig{ZoneID: "zone", Subdomain: "agent-job-001"},
			TTL:               "1h",
			CleanupOnFinish:   true,
		},
	}
	if mutate != nil {
		mutate(cr)
	}
	k8s := fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	h := &harness{
		t:   t,
		k8s: k8s,
		cf:  fake.New(),
		req: ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}},
		now: t0,
	}
	h.r = &AgentEdgeTunnelReconciler{
		Client:           k8s,
		Scheme:           scheme.Scheme,
		CloudflareClient: h.cf,
		BaseDomain:       "agents.example.com",
		// The harness uses the in-memory fake, which is what --dry-run runs on.
		DryRun: true,
		Now:    func() time.Time { return h.now },
	}
	return h
}

// reconcile runs one reconcile pass for the harness object.
func (h *harness) reconcile() (ctrl.Result, error) {
	return h.r.Reconcile(context.Background(), h.req)
}

// get returns the current stored copy of the harness object.
func (h *harness) get() agentopsv1alpha1.AgentEdgeTunnel {
	h.t.Helper()
	var got agentopsv1alpha1.AgentEdgeTunnel
	require.NoError(h.t, h.k8s.Get(context.Background(), h.req.NamespacedName, &got))
	return got
}

func TestProvisionsTunnel(t *testing.T) {
	h := newHarness(t, withTTL("1h"))

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.Equal(t, "agent-job-001.agents.example.com", got.Status.Hostname)
	assert.NotEmpty(t, got.Status.TunnelID)
	require.NotNil(t, got.Status.ExpiresAt)
	assert.True(t, got.Status.ExpiresAt.Time.Equal(t0.Add(time.Hour)))
	assert.Contains(t, got.Finalizers, FinalizerName)
	assert.Equal(t, got.Status.TunnelID, h.cf.DNSRecords["agent-job-001.agents.example.com"])
}

func TestTTLExpiryTerminatesAndDoesNotReprovision(t *testing.T) {
	h := newHarness(t, withTTL("1h"))
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 1, h.cf.CreateTunnelCalls)

	h.now = t0.Add(2 * time.Hour)
	_, err = h.reconcile()
	require.NoError(t, err)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, got.Status.Phase)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.DNSRecords)

	// The regression: a Terminated object used to be provisioned again.
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "terminated tunnel must not be re-created")
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, h.get().Status.Phase)
}

func TestRetryAfterDNSFailureDoesNotLeakTunnels(t *testing.T) {
	h := newHarness(t, withTTL(""))
	h.cf.CreateDNSErr = errors.New("cloudflare is down")

	_, err := h.reconcile()
	require.Error(t, err)
	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.NotEmpty(t, got.Status.TunnelID, "tunnel ID must be recorded before DNS is attempted")

	h.cf.CreateDNSErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "retry must reuse the existing tunnel")
	assert.Len(t, h.cf.Tunnels, 1)
	assert.Nil(t, h.get().Status.ExpiresAt, "empty ttl means no expiry")
}

func TestDeleteRunsCleanupViaFinalizer(t *testing.T) {
	h := newHarness(t, withTTL("1h"))
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Len(t, h.cf.Tunnels, 1)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))

	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.DNSRecords)
	var gone agentopsv1alpha1.AgentEdgeTunnel
	err = h.k8s.Get(context.Background(), h.req.NamespacedName, &gone)
	assert.True(t, apierrors.IsNotFound(err), "finalizer should be removed after cleanup")
}

func TestCleanupFailureKeepsFinalizer(t *testing.T) {
	h := newHarness(t, withTTL("1h"))
	_, err := h.reconcile()
	require.NoError(t, err)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	h.cf.DeleteTunnelErr = errors.New("cannot delete")

	_, err = h.reconcile()
	require.Error(t, err)
	still := h.get()
	assert.Contains(t, still.Finalizers, FinalizerName, "must not drop the finalizer while Cloudflare resources remain")
	assert.Len(t, h.cf.Tunnels, 1)

	// Once Cloudflare recovers, the retry finishes the job.
	h.cf.DeleteTunnelErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Empty(t, h.cf.Tunnels)
}

func TestInvalidTTLFailsWithoutCreatingTunnel(t *testing.T) {
	h := newHarness(t, withTTL("not-a-duration"))

	res, err := h.reconcile()
	require.NoError(t, err, "a spec error is not retried")
	assert.Zero(t, res.RequeueAfter)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "ttl")
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

func TestTunnelNameIsUniquePerNamespace(t *testing.T) {
	a := &agentopsv1alpha1.AgentEdgeTunnel{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a", UID: "aaaaaaaa-1111"}}
	b := &agentopsv1alpha1.AgentEdgeTunnel{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-b", UID: "bbbbbbbb-2222"}}
	assert.NotEqual(t, tunnelName(a), tunnelName(b))
}

func TestEmptyCloudflareSettingsFailBeforeCreatingAnything(t *testing.T) {
	// A resource without spec.cloudflare must never reach Cloudflare, even if the
	// CRD that should reject it is out of date.
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.Cloudflare = agentopsv1alpha1.CloudflareConfig{} })

	res, err := h.reconcile()
	require.NoError(t, err, "a spec error is not retried")
	assert.Zero(t, res.RequeueAfter)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "zoneID")
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

func TestRealModeDiscardsStatusWrittenByDryRun(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile() // dry-run provisions against the fake
	require.NoError(t, err)
	fakeID := h.get().Status.TunnelID
	require.True(t, strings.HasPrefix(fakeID, "dryrun-"), "fake IDs must be recognisable, got %q", fakeID)

	// The same object is now picked up by a real operator.
	h.r.DryRun = false
	res, err := h.reconcile()
	require.NoError(t, err)
	assert.True(t, res.Requeue, "state is reset and the object is reconciled again")

	got := h.get()
	assert.Empty(t, got.Status.TunnelID, "fake IDs must not stay authoritative")
	assert.Empty(t, got.Status.Phase, "the object must be provisioned again, not left Active")
	assert.Contains(t, h.cf.Tunnels, fakeID, "a real operator must not pretend to delete a fake tunnel")
}

func TestRealModeDeletesDryRunObjectWithoutPretendingToCleanUp(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	h.r.DryRun = false

	_, err = h.reconcile() // forgets the fake state
	require.NoError(t, err)
	_, err = h.reconcile() // nothing real to clean: releases the finalizer
	require.NoError(t, err)

	var gone agentopsv1alpha1.AgentEdgeTunnel
	assert.True(t, apierrors.IsNotFound(h.k8s.Get(context.Background(), h.req.NamespacedName, &gone)))
	assert.Len(t, h.cf.Tunnels, 1, "the fake tunnel was never touched by the real-mode operator")
}

func TestDryRunRefusesObjectsWithRealIDsAndKeepsFinalizer(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Finalizers = []string{FinalizerName}
		cr.Status = agentopsv1alpha1.AgentEdgeTunnelStatus{
			Phase: agentopsv1alpha1.PhaseActive, TunnelID: "6f1c2b3a-real", Hostname: "agent-job-001.agents.example.com",
		}
	})

	// Reconciling must not create, change or "clean up" anything.
	_, err := h.reconcile()
	require.Error(t, err)
	assert.Zero(t, h.cf.CreateTunnelCalls)
	assert.Equal(t, "6f1c2b3a-real", h.get().Status.TunnelID)

	// Deleting the object must not drop the finalizer: the fake would report
	// success without removing the real tunnel.
	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, h.get().Finalizers, FinalizerName, "real resources may still exist")
}

// flakyStatusClient makes the status write that records a freshly created
// tunnel report an error. With stored set, the write is applied first, like a
// request that timed out after the API server had accepted it.
type flakyStatusClient struct {
	client.Client
	stored bool
	failed bool
}

func (c *flakyStatusClient) Status() client.SubResourceWriter {
	return &flakyStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type flakyStatusWriter struct {
	client.SubResourceWriter
	c *flakyStatusClient
}

func (w *flakyStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	t, ok := obj.(*agentopsv1alpha1.AgentEdgeTunnel)
	if ok && !w.c.failed && t.Status.TunnelID != "" && t.Status.Phase == agentopsv1alpha1.PhasePending {
		w.c.failed = true
		if w.c.stored {
			if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
				return err
			}
		}
		return errors.New("etcdserver: request timed out")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestAmbiguousStatusWriteKeepsTheTunnel(t *testing.T) {
	for name, stored := range map[string]bool{"write lost": false, "write applied despite error": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.r.Client = &flakyStatusClient{Client: h.k8s, stored: stored}

			_, err := h.reconcile()
			require.Error(t, err)

			// Whether or not the write landed, deleting the tunnel could leave a
			// stored ID that names a tunnel that no longer exists.
			assert.Len(t, h.cf.Tunnels, 1)
			if stored {
				assert.NotEmpty(t, h.get().Status.TunnelID)
				_, err = h.reconcile()
				require.NoError(t, err)
				assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
				assert.Equal(t, 1, h.cf.CreateTunnelCalls, "the retry must reuse the stored tunnel")
				for id := range h.cf.Tunnels {
					assert.Equal(t, id, h.get().Status.TunnelID, "status must name a live tunnel")
				}
			}
		})
	}
}
