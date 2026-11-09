package controller

import (
	"context"
	"testing"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNamespaceQuotaAbsentMeansUnlimited(t *testing.T) {
	h := newHarness(t, nil)
	tunnel := h.get()
	require.NoError(t, h.r.ensureQuotaReservation(context.Background(), &tunnel, false))
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 1, h.cf.CreateTunnelCalls)
}

func TestNamespaceQuotaRejectsAdmissionBeforeCloudflareCreate(t *testing.T) {
	quota := &agentopsv1alpha1.NamespaceTunnelQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentopsv1alpha1.NamespaceTunnelQuotaSpec{Limit: 0},
	}
	h := newHarness(t, nil, quota)
	_, err := h.reconcile()
	require.ErrorContains(t, err, "quota exhausted")
	require.Zero(t, h.cf.CreateTunnelCalls, "quota must be checked before creating Cloudflare resources")
	require.Zero(t, h.cf.CreateAccessCalls)
}

func TestNamespaceQuotaAdmissionIsIdempotentAndReleased(t *testing.T) {
	quota := &agentopsv1alpha1.NamespaceTunnelQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentopsv1alpha1.NamespaceTunnelQuotaSpec{Limit: 1},
	}
	h := newHarness(t, nil, quota)
	tunnel := h.get()
	require.NoError(t, h.r.ensureQuotaReservation(context.Background(), &tunnel, false))
	require.NoError(t, h.r.ensureQuotaReservation(context.Background(), &tunnel, false), "retry must not consume another slot")

	second := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "default", UID: types.UID("22222222-aaaa-bbbb-cccc-333333333333")},
		Spec: tunnel.Spec,
	}
	require.NoError(t, h.k8s.Create(context.Background(), second))
	require.ErrorContains(t, h.r.ensureQuotaReservation(context.Background(), second, false), "quota exhausted")

	require.NoError(t, h.r.releaseQuotaReservation(context.Background(), &tunnel))
	require.NoError(t, h.r.ensureQuotaReservation(context.Background(), second, false), "released slot should be reusable")

	var stored agentopsv1alpha1.NamespaceTunnelQuota
	require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace:"default", Name:"default"}, &stored))
	require.Len(t, stored.Status.Reservations, 1)
	require.Equal(t, string(second.UID), stored.Status.Reservations[0].UID)
}

func TestNamespaceQuotaGrandfathersExistingTunnelsWhenLimitDrops(t *testing.T) {
	quota := &agentopsv1alpha1.NamespaceTunnelQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentopsv1alpha1.NamespaceTunnelQuotaSpec{Limit: 0},
	}
	h := newHarness(t, func(tunnel *agentopsv1alpha1.AgentEdgeTunnel) {
		tunnel.Status.TunnelID = "already-created"
		tunnel.Status.Phase = agentopsv1alpha1.PhaseActive
	}, quota)
	tunnel := h.get()
	require.NoError(t, h.r.ensureQuotaReservation(context.Background(), &tunnel, true), "existing tunnels are grandfathered")
	require.ErrorContains(t, h.r.ensureQuotaReservation(context.Background(), &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name:"new", Namespace:"default", UID:types.UID("33333333-aaaa-bbbb-cccc-444444444444")},
	}, false), "quota exhausted")
}
