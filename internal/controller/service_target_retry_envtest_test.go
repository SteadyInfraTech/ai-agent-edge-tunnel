package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cfake "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare/fake"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stretchr/testify/require"
)

// TestServiceTargetRetryManagerEnvtest exercises the real manager watches and
// work queue. In particular, the status update that records the first
// transient mismatch must not cause rapid reconciles to exhaust the grace
// period before EndpointSlice convergence completes.
func TestServiceTargetRetryManagerEnvtest(t *testing.T) {
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
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)

	cloudflare := cfake.New()
	reconciler := &AgentEdgeTunnelReconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		Scheme:           scheme,
		CloudflareClient: cloudflare,
		AccountID:        "fake-account",
		ZoneID:           "zone",
		BaseDomain:       "agents.example.com",
		ConnectorImage:   testImage,
		DryRun:            true,
	}
	require.NoError(t, reconciler.SetupWithManager(mgr))

	managerCtx, cancelManager := context.WithCancel(context.Background())
	managerErr := make(chan error, 1)
	go func() {
		managerErr <- mgr.Start(managerCtx)
	}()
	require.True(t, mgr.GetCache().WaitForCacheSync(managerCtx), "manager cache should synchronize")
	t.Cleanup(func() {
		cancelManager()
		select {
		case err := <-managerErr:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})

	k8s := mgr.GetClient()
	ctx := context.Background()
	require.NoError(t, k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
	for _, obj := range serviceTestFixtures("default", "langgraph-worker", "10.96.0.10", "10.244.0.10") {
		require.NoError(t, k8s.Create(ctx, obj))
	}

	tunnel := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "manager-watch-test", Namespace: "default"},
		Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
			Service:    &agentopsv1alpha1.ServiceTarget{Name: "langgraph-worker", Port: 8080},
			Cloudflare: agentopsv1alpha1.CloudflareConfig{ZoneID: "zone", Subdomain: "manager-watch-test"},
			TTL:        "1h",
			Security:   agentopsv1alpha1.SecurityConfig{AllowedEmails: []string{"dev@example.com"}},
		},
	}
	require.NoError(t, k8s.Create(ctx, tunnel))

	getTunnel := func() agentopsv1alpha1.AgentEdgeTunnel {
		t.Helper()
		var got agentopsv1alpha1.AgentEdgeTunnel
		require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(tunnel), &got))
		return got
	}
	require.Eventually(t, func() bool {
		return getTunnel().Status.Phase == agentopsv1alpha1.PhaseActive
	}, 30*time.Second, 100*time.Millisecond, "the manager should provision the tunnel")

	var slice discoveryv1.EndpointSlice
	sliceKey := client.ObjectKey{Namespace: "default", Name: "langgraph-worker-slice"}
	require.NoError(t, k8s.Get(ctx, sliceKey, &slice))
	slice.Endpoints[0].Addresses = []string{"192.0.2.10"}
	require.NoError(t, k8s.Update(ctx, &slice))

	require.Eventually(t, func() bool {
		got := getTunnel()
		return got.Status.Phase == agentopsv1alpha1.PhaseActive && got.Status.ServiceTargetVerificationSince != nil
	}, 10*time.Second, 100*time.Millisecond, "the first transient mismatch should be recorded in status")
	startedAt := getTunnel().Status.ServiceTargetVerificationSince
	require.NotNil(t, startedAt)

	// Allow the status update, Service/EndpointSlice watches, and at least one
	// delayed retry to enqueue reconciliations while the mismatch is present.
	time.Sleep(3 * time.Second)
	got := getTunnel()
	require.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	require.Equal(t, startedAt, got.Status.ServiceTargetVerificationSince)

	// Simulate normal EndpointSlice convergence before the 30-second deadline.
	require.NoError(t, k8s.Get(ctx, sliceKey, &slice))
	slice.Endpoints[0].Addresses = []string{"10.244.0.10"}
	require.NoError(t, k8s.Update(ctx, &slice))
	require.Eventually(t, func() bool {
		got := getTunnel()
		return got.Status.Phase == agentopsv1alpha1.PhaseActive && got.Status.ServiceTargetVerificationSince == nil
	}, 10*time.Second, 100*time.Millisecond, "convergence should clear the grace-period timestamp without terminating the tunnel")
}
