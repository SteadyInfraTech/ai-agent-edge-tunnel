package controller

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stretchr/testify/require"
)

type quotaGetBarrier struct {
	client.Reader
	arrivals chan struct{}
	release  <-chan struct{}
	calls    atomic.Int32
}

// Get pauses the first two quota reads after fetching them, forcing both callers
// to attempt a status update using the same resourceVersion.
func (r *quotaGetBarrier) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*agentopsv1alpha1.NamespaceTunnelQuota); ok && r.calls.Add(1) <= 2 {
		r.arrivals <- struct{}{}
		<-r.release
	}
	return err
}

// TestNamespaceQuotaEnvtest verifies quota admission against a real Kubernetes API server.
func TestNamespaceQuotaEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets are installed only by the dedicated quota-envtest workflow")
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, testEnv.Stop())
	})

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme))
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := context.Background()

	createNamespace := func(name string) {
		t.Helper()
		require.NoError(t, k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}
	createQuota := func(namespace string, limit int32) *agentopsv1alpha1.NamespaceTunnelQuota {
		t.Helper()
		quota := &agentopsv1alpha1.NamespaceTunnelQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace},
			Spec:       agentopsv1alpha1.NamespaceTunnelQuotaSpec{Limit: limit},
		}
		require.NoError(t, k8s.Create(ctx, quota))
		return quota
	}
	createTunnel := func(namespace, name string) *agentopsv1alpha1.AgentEdgeTunnel {
		t.Helper()
		tunnel := &agentopsv1alpha1.AgentEdgeTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
				Cloudflare: agentopsv1alpha1.CloudflareConfig{ZoneID: "zone", Subdomain: name},
			},
		}
		require.NoError(t, k8s.Create(ctx, tunnel))
		return tunnel
	}
	newReconciler := func(reader client.Reader) *AgentEdgeTunnelReconciler {
		return &AgentEdgeTunnelReconciler{Client: k8s, APIReader: reader}
	}

	// (a) Two independent reconcilers race for one slot. The barrier guarantees
	// both read the same quota resourceVersion before either writes status.
	createNamespace("quota-race")
	createQuota("quota-race", 1)
	raceTunnels := []*agentopsv1alpha1.AgentEdgeTunnel{
		createTunnel("quota-race", "concurrent-a"),
		createTunnel("quota-race", "concurrent-b"),
	}
	release := make(chan struct{})
	barrier := &quotaGetBarrier{Reader: k8s, arrivals: make(chan struct{}, 2), release: release}
	reconcilers := []*AgentEdgeTunnelReconciler{newReconciler(barrier), newReconciler(barrier)}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, tunnel := range raceTunnels {
		wg.Add(1)
		go func(r *AgentEdgeTunnelReconciler, tunnel *agentopsv1alpha1.AgentEdgeTunnel) {
			defer wg.Done()
			<-start
			errs <- r.ensureQuotaReservation(ctx, tunnel, false)
		}(reconcilers[i], tunnel)
	}
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.arrivals:
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for both callers to read the same quota resourceVersion")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	admitted, rejected := 0, 0
	for err := range errs {
		if err == nil {
			admitted++
		} else {
			require.ErrorIs(t, err, errQuotaExhausted)
			rejected++
		}
	}
	require.Equal(t, 1, admitted)
	require.Equal(t, 1, rejected)
	require.GreaterOrEqual(t, barrier.calls.Load(), int32(3), "one caller must retry after the resourceVersion conflict")
	var raceQuota agentopsv1alpha1.NamespaceTunnelQuota
	require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "quota-race", Name: "default"}, &raceQuota))
	require.Len(t, raceQuota.Status.Reservations, 1)

	// (b) Retrying the same object before its Cloudflare ID is recorded is idempotent.
	createNamespace("quota-retry")
	createQuota("quota-retry", 1)
	first := createTunnel("quota-retry", "retry-first")
	reconciler := newReconciler(k8s)
	require.NoError(t, reconciler.ensureQuotaReservation(ctx, first, false))
	require.NoError(t, reconciler.ensureQuotaReservation(ctx, first, false))
	second := createTunnel("quota-retry", "retry-second")
	require.ErrorIs(t, reconciler.ensureQuotaReservation(ctx, second, false), errQuotaExhausted)
	var retryQuota agentopsv1alpha1.NamespaceTunnelQuota
	require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "quota-retry", Name: "default"}, &retryQuota))
	require.Len(t, retryQuota.Status.Reservations, 1)

	// (c) Existing provisioned tunnels remain admitted when the limit is lowered.
	createNamespace("quota-lower")
	quota := createQuota("quota-lower", 2)
	existing := createTunnel("quota-lower", "existing")
	existing.Status.TunnelID = "already-provisioned"
	existing.Status.Phase = agentopsv1alpha1.PhaseActive
	require.NoError(t, k8s.Status().Update(ctx, existing))
	require.NoError(t, reconciler.ensureQuotaReservation(ctx, existing, true))
	require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(quota), quota))
	quota.Spec.Limit = 0
	require.NoError(t, k8s.Update(ctx, quota))
	require.NoError(t, reconciler.ensureQuotaReservation(ctx, existing, true), "existing provisioned tunnels are grandfathered")
	newTunnel := createTunnel("quota-lower", "new-after-lowering")
	require.ErrorIs(t, reconciler.ensureQuotaReservation(ctx, newTunnel, false), errQuotaExhausted)
}
