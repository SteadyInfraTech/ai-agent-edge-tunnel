package controller

import (
	"context"
	"errors"
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

type harness struct {
	t   *testing.T
	k8s client.Client
	cf  *fake.Client
	r   *AgentEdgeTunnelReconciler
	req ctrl.Request
	now time.Time
}

func newHarness(t *testing.T, ttl string) *harness {
	t.Helper()
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme.Scheme))

	cr := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent-tunnel", Namespace: "default"},
		Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
			TargetPodSelector: map[string]string{"app": "langgraph-agent"},
			Cloudflare:        agentopsv1alpha1.CloudflareConfig{ZoneID: "zone", Subdomain: "agent-job-001"},
			TTL:               ttl,
			CleanupOnFinish:   true,
		},
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
		Now:              func() time.Time { return h.now },
	}
	return h
}

func (h *harness) reconcile() (ctrl.Result, error) {
	return h.r.Reconcile(context.Background(), h.req)
}

func (h *harness) get() agentopsv1alpha1.AgentEdgeTunnel {
	h.t.Helper()
	var got agentopsv1alpha1.AgentEdgeTunnel
	require.NoError(h.t, h.k8s.Get(context.Background(), h.req.NamespacedName, &got))
	return got
}

func TestProvisionsTunnel(t *testing.T) {
	h := newHarness(t, "1h")

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
	h := newHarness(t, "1h")
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
	h := newHarness(t, "")
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
	h := newHarness(t, "1h")
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
	h := newHarness(t, "1h")
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
	h := newHarness(t, "not-a-duration")

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
