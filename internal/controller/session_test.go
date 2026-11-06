package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
)

const sessionUID = "9d2f0c1e-0000-4000-8000-000000000001"

// newSession builds an AgentSession the way agentops-operator publishes it.
func newSession(phase, endpoint string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"phase": phase, "endpoint": endpoint, "assignedAgent": "support-agent"},
	}}
	u.SetGroupVersionKind(agentSessionGVK)
	u.SetName("session-1")
	u.SetNamespace("default")
	u.SetUID(types.UID(sessionUID))
	return u
}

// withSession makes the harness object expose a session instead of a Service.
func withSession(cr *agentopsv1alpha1.AgentEdgeTunnel) {
	cr.Spec.Service = nil
	cr.Spec.SessionRef = &agentopsv1alpha1.SessionReference{Name: "session-1"}
}

// setSession rewrites the stored session.
func setSession(t *testing.T, h *harness, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(agentSessionGVK)
	require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "session-1"}, u))
	mutate(u)
	require.NoError(t, h.k8s.Update(context.Background(), u))
}

// TestSessionRefWaitsForBound checks: nothing is created in Cloudflare until
// the session is Bound.
func TestSessionRefWaitsForBound(t *testing.T) {
	h := newHarness(t, withSession, newSession("Pending", ""))

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, sessionPollInterval, res.RequeueAfter)
	assert.Equal(t, agentopsv1alpha1.PhasePending, h.get().Status.Phase)
	assert.Contains(t, h.get().Status.Message, "Bound")
	assert.Zero(t, h.cf.CreateTunnelCalls)
	assert.Empty(t, h.get().Status.SessionUID)
}

// TestSessionRefWaitsForMissingSession checks: a session that does not exist
// yet is waited for, not failed, so the two objects can be applied in either
// order.
func TestSessionRefWaitsForMissingSession(t *testing.T) {
	h := newHarness(t, withSession)

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, sessionPollInterval, res.RequeueAfter)
	assert.Equal(t, agentopsv1alpha1.PhasePending, h.get().Status.Phase)
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestSessionRefProvisionsFromTheSessionEndpoint checks: once Bound, the
// session is pinned and the connector targets the session's endpoint.
func TestSessionRefProvisionsFromTheSessionEndpoint(t *testing.T) {
	h := newHarness(t, withSession, newSession("Bound", "http://support-agent.default.svc:8080"))

	_, err := h.reconcile()
	require.NoError(t, err)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.Equal(t, sessionUID, got.Status.SessionUID)
	assert.Equal(t, "http://support-agent.default.svc:8080", got.Status.SessionEndpoint)
	assert.Nil(t, got.Spec.Service, "the spec must not be rewritten")

	sec, err := h.secret()
	require.NoError(t, err)
	assert.Contains(t, string(sec.Data[configKey]), "service: http://support-agent.default.svc:8080")
}

// TestSessionRefTerminatesWhenSessionEnds checks: Completed, Expired and Failed
// sessions remove the tunnel and the connector.
func TestSessionRefTerminatesWhenSessionEnds(t *testing.T) {
	for _, phase := range []string{"Completed", "Expired", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, withSession, newSession("Bound", "http://support-agent.default.svc:8080"))
			_, err := h.reconcile()
			require.NoError(t, err)
			require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)

			setSession(t, h, func(u *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedField(u.Object, phase, "status", "phase"))
			})
			_, err = h.reconcile()
			require.NoError(t, err)

			got := h.get()
			assert.Equal(t, agentopsv1alpha1.PhaseTerminated, got.Status.Phase)
			assert.Contains(t, got.Status.Message, phase)
			assert.Empty(t, h.cf.Tunnels)
			assert.Empty(t, h.cf.DNSRecords)
			assert.Empty(t, h.cf.AccessApps)
			h.assertConnectorGone()
		})
	}
}

// TestSessionRefTerminatesWhenSessionIsDeleted checks: a bound session that
// disappears ends the tunnel.
func TestSessionRefTerminatesWhenSessionIsDeleted(t *testing.T) {
	h := newHarness(t, withSession, newSession("Bound", "http://support-agent.default.svc:8080"))
	_, err := h.reconcile()
	require.NoError(t, err)

	require.NoError(t, h.k8s.Delete(context.Background(), newSession("", "")))
	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, h.get().Status.Phase)
	assert.Empty(t, h.cf.Tunnels)
	h.assertConnectorGone()
}

// TestSessionRefDoesNotInheritAReplacedSession checks: a session deleted and
// recreated under the same name has a new UID and must not keep the old
// tunnel and its Access policy.
func TestSessionRefDoesNotInheritAReplacedSession(t *testing.T) {
	h := newHarness(t, withSession, newSession("Bound", "http://support-agent.default.svc:8080"))
	_, err := h.reconcile()
	require.NoError(t, err)

	setSession(t, h, func(u *unstructured.Unstructured) { u.SetUID("ffffffff-0000-4000-8000-00000000000f") })
	_, err = h.reconcile()
	require.NoError(t, err)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "replaced")
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.AccessApps)
}

// TestSessionRefTerminatesOnRebind checks: a session bound to a different
// endpoint ends the tunnel instead of silently re-pointing it.
func TestSessionRefTerminatesOnRebind(t *testing.T) {
	h := newHarness(t, withSession, newSession("Bound", "http://support-agent.default.svc:8080"))
	_, err := h.reconcile()
	require.NoError(t, err)

	setSession(t, h, func(u *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(u.Object, "http://other-agent.default.svc:8080", "status", "endpoint"))
	})
	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, h.get().Status.Phase)
	assert.Empty(t, h.cf.Tunnels)
}

// TestSessionRefRejectsEndpointsOutsideTheNamespace checks: a session cannot
// point a tunnel at another namespace or at an arbitrary host.
func TestSessionRefRejectsEndpointsOutsideTheNamespace(t *testing.T) {
	for name, ep := range map[string]string{
		"other namespace": "http://agent.kube-system.svc:8080",
		"external host":   "http://evil.example.com:8080",
		"https":           "https://agent.default.svc:8080",
		"with a path":     "http://agent.default.svc:8080/admin",
		"with userinfo":   "http://u:p@agent.default.svc:8080",
		"empty":           "",
		"bad port":        "http://agent.default.svc:99999",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withSession, newSession("Bound", ep))

			res, err := h.reconcile()
			require.NoError(t, err)
			assert.Equal(t, sessionPollInterval, res.RequeueAfter)
			assert.Zero(t, h.cf.CreateTunnelCalls, "nothing may be created for an endpoint we will not expose")
			assert.Empty(t, h.get().Status.SessionUID)
		})
	}
}

// TestSessionRefAndServiceAreMutuallyExclusive checks: both set is a spec error.
func TestSessionRefAndServiceAreMutuallyExclusive(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Spec.SessionRef = &agentopsv1alpha1.SessionReference{Name: "session-1"}
	}, newSession("Bound", "http://support-agent.default.svc:8080"))

	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestDeletingASessionTunnelRemovesTheConnector checks: deletion cleans up the
// connector for a sessionRef tunnel; otherwise the tunnel could not be deleted
// while its connector still holds a connection.
func TestDeletingASessionTunnelRemovesTheConnector(t *testing.T) {
	h := newHarness(t, withSession, newSession("Bound", "http://support-agent.default.svc:8080"))
	_, err := h.reconcile()
	require.NoError(t, err)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Empty(t, h.cf.Tunnels)
	h.assertConnectorGone()
}
