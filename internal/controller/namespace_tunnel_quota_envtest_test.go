package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stretchr/testify/require"
)

// quotaGetBarrier makes concurrent callers observe the same quota resourceVersion.
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
	invalidQuota := &agentopsv1alpha1.NamespaceTunnelQuota{ObjectMeta: metav1.ObjectMeta{Name: "not-default", Namespace: "quota-race"}, Spec: agentopsv1alpha1.NamespaceTunnelQuotaSpec{Limit: 1}}
	require.Error(t, k8s.Create(ctx, invalidQuota), "the CRD must reject non-default quota names")
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
			close(release)
			wg.Wait()
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

// TestTenantAdmissionPolicyEnvtest exercises the checked-in admission policy
// against a real API server, including zone pinning and the exact public namespace allowlist.
func TestTenantAdmissionPolicyEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets are installed only by the dedicated envtest workflow")
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
	require.NoError(t, admissionregistrationv1.AddToScheme(scheme))
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme))
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, name := range []string{"edge-tunnel-system", "allowed-public", "restricted"} {
		require.NoError(t, k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}
	params := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-tunnel-config", Namespace: "edge-tunnel-system"},
		Data: map[string]string{
			"CLOUDFLARE_ZONE_ID":     "zone-123",
			"CLOUDFLARE_ZONE_NAME":   "example.com",
			"EDGE_BASE_DOMAIN":       "agents.example.com",
			"EDGE_PUBLIC_NAMESPACES": "allowed-public, another-public",
		},
	}
	require.NoError(t, k8s.Create(ctx, params))

	policyYAML, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", "validatingadmissionpolicy.yaml"))
	require.NoError(t, err)
	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	require.NoError(t, yaml.Unmarshal(policyYAML, &policy))
	require.NoError(t, k8s.Create(ctx, &policy))

	bindingYAML, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", "validatingadmissionpolicybinding.yaml"))
	require.NoError(t, err)
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, yaml.Unmarshal(bindingYAML, &binding))
	require.NoError(t, k8s.Create(ctx, &binding))

	// The API server may need a short interval to observe the policy and binding.
	// Probe enforcement itself instead of depending on status controllers that
	// envtest does not necessarily run.
	newTunnel := func(namespace, name, zoneID string, allowPublic bool) *agentopsv1alpha1.AgentEdgeTunnel {
		return &agentopsv1alpha1.AgentEdgeTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
				Cloudflare: agentopsv1alpha1.CloudflareConfig{ZoneID: zoneID, Subdomain: name},
				Security:   agentopsv1alpha1.SecurityConfig{AllowPublic: allowPublic},
			},
		}
	}

	zonePolicyMessage := "spec.cloudflare.zoneID must match the configured Cloudflare zone for EDGE_BASE_DOMAIN"
	isZonePolicyDenial := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), zonePolicyMessage)
	}
	probePolicyUntilDenied := func(zoneID, label string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			probe := newTunnel("restricted", fmt.Sprintf("%s-%d", label, time.Now().UnixNano()), zoneID, false)
			err := k8s.Create(ctx, probe)
			if isZonePolicyDenial(err) {
				return
			}
			if err == nil {
				require.NoError(t, k8s.Delete(ctx, probe), "remove a probe admitted before policy activation")
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for the expected admission denial: %s", zonePolicyMessage)
	}
	probePolicyUntilDenied("other-zone", "wrong-zone-probe")

	require.NoError(t, k8s.Create(ctx, newTunnel("restricted", "valid-protected", "zone-123", false)))
	require.Error(t, k8s.Create(ctx, newTunnel("restricted", "wrong-zone", "other-zone", false)),
		"a tenant must not select a different Cloudflare zone")
	require.Error(t, k8s.Create(ctx, newTunnel("restricted", "public-denied", "zone-123", true)),
		"allowPublic must be denied outside the configured namespace allowlist")
	require.NoError(t, k8s.Create(ctx, newTunnel("allowed-public", "public-allowed", "zone-123", true)),
		"allowPublic must be accepted in an exactly allowlisted namespace")

	// Prove the API server has observed each ConfigMap update before checking
	// normalization; otherwise a stale, previously-valid parameter could mask a bug.
	params.Data["CLOUDFLARE_ZONE_NAME"] = ".example.com"
	params.Data["EDGE_BASE_DOMAIN"] = "agents.other.example"
	require.NoError(t, k8s.Update(ctx, params))
	probePolicyUntilDenied("zone-123", "wrong-base-domain-probe")

	// The operator accepts one leading dot on the zone name; admission must
	// normalize it identically rather than rejecting otherwise-valid tenants.
	params.Data["EDGE_BASE_DOMAIN"] = "agents.example.com"
	require.NoError(t, k8s.Update(ctx, params))
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		probe := newTunnel("restricted", fmt.Sprintf("leading-dot-zone-probe-%d", time.Now().UnixNano()), "zone-123", false)
		err := k8s.Create(ctx, probe)
		if err == nil {
			require.NoError(t, k8s.Delete(ctx, probe))
			break
		}
		if !isZonePolicyDenial(err) {
			t.Fatalf("unexpected error while waiting for normalized zone parameters: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, k8s.Create(ctx, newTunnel("restricted", "leading-dot-zone-name", "zone-123", false)))
	params.Data["CLOUDFLARE_ZONE_NAME"] = "example.com"

	// A malformed base-domain/zone-name tuple must fail closed.

	// A terminating object must be able to drop its finalizer even if the
	// parameter ConfigMap disappears; ordinary CREATE requests remain denied.
	params.Data["EDGE_BASE_DOMAIN"] = "agents.example.com"
	require.NoError(t, k8s.Update(ctx, params))
	terminating := newTunnel("restricted", "finalizer-cleanup", "zone-123", false)
	terminating.Finalizers = []string{FinalizerName}
	require.NoError(t, k8s.Create(ctx, terminating))
	require.NoError(t, k8s.Delete(ctx, terminating))
	require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(terminating), terminating))
	require.NotNil(t, terminating.DeletionTimestamp)
	require.NoError(t, k8s.Delete(ctx, params))
	var ordinaryUpdate agentopsv1alpha1.AgentEdgeTunnel
	require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "restricted", Name: "valid-protected"}, &ordinaryUpdate))
	ordinaryUpdate.Annotations = map[string]string{"tenant-change": "blocked"}
	require.Error(t, k8s.Update(ctx, &ordinaryUpdate), "ordinary updates must remain fail-closed when policy parameters are missing")
	terminating.Finalizers = nil
	require.NoError(t, k8s.Update(ctx, terminating), "finalizer removal must bypass invalid or missing policy parameters")
	missingParamsErr := k8s.Create(ctx, newTunnel("restricted", "missing-policy-params", "zone-123", false))
	require.Error(t, missingParamsErr, "ordinary creates must remain fail-closed when the parameter ConfigMap is missing")
}
