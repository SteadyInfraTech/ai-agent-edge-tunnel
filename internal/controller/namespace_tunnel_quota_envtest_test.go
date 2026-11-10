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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			"CLOUDFLARE_ZONE_ID":      "zone-123",
			"CLOUDFLARE_ZONE_NAME":    "example.com",
			"EDGE_BASE_DOMAIN":        "agents.example.com",
			"EDGE_PUBLIC_NAMESPACES":  "allowed-public, another-public",
			"EDGE_QUOTA_ADMIN_GROUPS": "quota-admins",
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

	quotaPolicyYAML, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", "namespacequota_validatingadmissionpolicy.yaml"))
	require.NoError(t, err)
	var quotaPolicy admissionregistrationv1.ValidatingAdmissionPolicy
	require.NoError(t, yaml.Unmarshal(quotaPolicyYAML, &quotaPolicy))
	require.NoError(t, k8s.Create(ctx, &quotaPolicy))

	quotaBindingYAML, err := os.ReadFile(filepath.Join("..", "..", "config", "admission", "namespacequota_validatingadmissionpolicybinding.yaml"))
	require.NoError(t, err)
	var quotaBinding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, yaml.Unmarshal(quotaBindingYAML, &quotaBinding))
	require.NoError(t, k8s.Create(ctx, &quotaBinding))

	// Namespace quota limits are an administrative capacity decision. A
	// tenant with broad custom-resource write access must not be able to
	// create, raise, or remove its own quota. Configured admin groups can.
	tenantUser, err := testEnv.AddUser("quota-tenant", []string{"tenant-users"}, cfg)
	require.NoError(t, err)
	tenantClient, err := client.New(tenantUser.Config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	quotaAdmin, err := testEnv.AddUser("quota-admin", []string{"quota-admins"}, cfg)
	require.NoError(t, err)
	quotaAdminClient, err := client.New(quotaAdmin.Config, client.Options{Scheme: scheme})
	require.NoError(t, err)

	quotaNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "quota-policy"}}
	require.NoError(t, k8s.Create(ctx, quotaNamespace))
	quotaObject := func() *agentopsv1alpha1.NamespaceTunnelQuota {
		return &agentopsv1alpha1.NamespaceTunnelQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "quota-policy"},
			Spec:       agentopsv1alpha1.NamespaceTunnelQuotaSpec{Limit: 1},
		}
	}
	quotaCreateDenied := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		attempt := quotaObject()
		createErr := tenantClient.Create(ctx, attempt)
		if apierrors.IsForbidden(createErr) || apierrors.IsInvalid(createErr) {
			quotaCreateDenied = true
			break
		}
		if createErr != nil {
			t.Fatalf("unexpected error while waiting for quota admin policy: %v", createErr)
		}
		require.NoError(t, k8s.Delete(ctx, attempt), "remove quota admitted before policy activation")
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, quotaCreateDenied, "tenant users must not create namespace quota objects")

	quota := quotaObject()
	require.NoError(t, quotaAdminClient.Create(ctx, quota), "configured quota admin group should create quotas")
	var tenantView agentopsv1alpha1.NamespaceTunnelQuota
	require.NoError(t, tenantClient.Get(ctx, client.ObjectKeyFromObject(quota), &tenantView))
	tenantView.Spec.Limit = 1000
	require.Error(t, tenantClient.Update(ctx, &tenantView), "tenant users must not raise their own quota")
	tenantView.Status.Reservations = []agentopsv1alpha1.TunnelReservation{{Name: "forged", UID: "forged-uid"}}
	require.Error(t, tenantClient.Status().Update(ctx, &tenantView), "tenant users must not forge quota reservations")
	require.Error(t, quotaAdminClient.Status().Update(ctx, &tenantView), "quota admins may manage limits but not the operator-owned reservation ledger")
	require.NoError(t, k8s.Status().Update(ctx, &tenantView), "system:masters must retain break-glass status access")
	require.Error(t, tenantClient.Delete(ctx, quota), "tenant users must not delete namespace quotas")
	// The failed delete must leave the quota in place; a configured admin can
	// update and delete it.
	require.NoError(t, quotaAdminClient.Get(ctx, client.ObjectKeyFromObject(quota), quota))
	quota.Spec.Limit = 2
	require.NoError(t, quotaAdminClient.Update(ctx, quota))
	require.NoError(t, quotaAdminClient.Delete(ctx, quota))

	// A missing parameter ConfigMap must not lock out cluster administrators,
	// but configured namespace-quota admin groups must fail closed.
	require.NoError(t, k8s.Delete(ctx, params))
	breakGlassQuota := quotaObject()
	require.NoError(t, k8s.Create(ctx, breakGlassQuota), "system:masters must retain quota recovery access without policy parameters")
	require.Error(t, tenantClient.Create(ctx, quotaObject()), "non-admin groups must remain denied when policy parameters are missing")
	require.NoError(t, k8s.Delete(ctx, breakGlassQuota))
	params = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-tunnel-config", Namespace: "edge-tunnel-system"},
		Data: map[string]string{
			"CLOUDFLARE_ZONE_ID":      "zone-123",
			"CLOUDFLARE_ZONE_NAME":    "example.com",
			"EDGE_BASE_DOMAIN":        "agents.example.com",
			"EDGE_PUBLIC_NAMESPACES":  "allowed-public, another-public",
			"EDGE_QUOTA_ADMIN_GROUPS": "quota-admins",
		},
	}
	require.NoError(t, k8s.Create(ctx, params))

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
	params.Data["EDGE_BASE_DOMAIN"] = ""
	require.NoError(t, k8s.Update(ctx, params))
	probePolicyUntilDenied("zone-123", "empty-base-domain-probe")
	params.Data["EDGE_BASE_DOMAIN"] = "agents.example.com"
	require.NoError(t, k8s.Update(ctx, params))

	// A terminating object can remove its finalizer when parameters exist,
	// even if their values are invalid. Missing parameters remain fail-closed.
	terminating := newTunnel("restricted", "finalizer-cleanup", "zone-123", false)
	terminating.Finalizers = []string{FinalizerName}
	require.NoError(t, k8s.Create(ctx, terminating))
	require.NoError(t, k8s.Delete(ctx, terminating))
	require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(terminating), terminating))
	require.NotNil(t, terminating.DeletionTimestamp)
	require.NoError(t, k8s.Delete(ctx, params))

	// Parameter resolution is cached by the API server. Wait until the deleted
	// ConfigMap is no longer served as policy parameters before asserting that
	// both ordinary updates and creates are denied.
	isMissingParameterDenial := func(err error) bool {
		return apierrors.IsForbidden(err) || apierrors.IsInvalid(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	updateDenied := false
	for time.Now().Before(deadline) {
		var ordinaryUpdate agentopsv1alpha1.AgentEdgeTunnel
		require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "restricted", Name: "valid-protected"}, &ordinaryUpdate))
		ordinaryUpdate.Annotations = map[string]string{"tenant-change": fmt.Sprintf("%d", time.Now().UnixNano())}
		updateErr := k8s.Update(ctx, &ordinaryUpdate)
		if isMissingParameterDenial(updateErr) {
			updateDenied = true
			break
		}
		if updateErr != nil {
			t.Fatalf("unexpected error while waiting for missing policy parameters to take effect: %v", updateErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, updateDenied, "ordinary updates must remain fail-closed when policy parameters are missing")

	createDenied := false
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		probe := newTunnel("restricted", fmt.Sprintf("missing-policy-params-%d", time.Now().UnixNano()), "zone-123", false)
		createErr := k8s.Create(ctx, probe)
		if isMissingParameterDenial(createErr) {
			createDenied = true
			break
		}
		if createErr != nil {
			t.Fatalf("unexpected error while waiting for missing policy parameters to take effect: %v", createErr)
		}
		require.NoError(t, k8s.Delete(ctx, probe), "remove a probe admitted before missing parameters take effect")
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, createDenied, "ordinary creates must remain fail-closed when the parameter ConfigMap is missing")

	// Restore the ConfigMap with an invalid zone ID. Admission remains
	// fail-closed for finalizer removal until the configuration is valid again.
	restoredParams := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-tunnel-config", Namespace: "edge-tunnel-system"},
		Data: map[string]string{
			"CLOUDFLARE_ZONE_ID":     "different-zone",
			"CLOUDFLARE_ZONE_NAME":    "example.com",
			"EDGE_BASE_DOMAIN":        "agents.example.com",
			"EDGE_PUBLIC_NAMESPACES":  "allowed-public, another-public",
		},
	}
	require.NoError(t, k8s.Create(ctx, restoredParams))
	probePolicyUntilDenied("zone-123", "wrong-zone-after-restore")
	var current agentopsv1alpha1.AgentEdgeTunnel
	require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(terminating), &current))
	current.Finalizers = nil
	var finalizerRemovalDenied bool
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		removeErr := k8s.Update(ctx, &current)
		if apierrors.IsForbidden(removeErr) || apierrors.IsInvalid(removeErr) {
			finalizerRemovalDenied = true
			break
		}
		if removeErr != nil {
			t.Fatalf("unexpected error while checking invalid-parameter cleanup behavior: %v", removeErr)
		}
		t.Fatalf("finalizer removal unexpectedly succeeded with an invalid zone ID")
	}
	require.True(t, finalizerRemovalDenied, "finalizer removal must remain blocked while admission parameters are invalid")

	// Repair the parameter ConfigMap, then the terminating resource can finish.
	restoredParams.Data["CLOUDFLARE_ZONE_ID"] = "zone-123"
	require.NoError(t, k8s.Update(ctx, restoredParams))
	finalizerRemoved := false
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var latest agentopsv1alpha1.AgentEdgeTunnel
		require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(terminating), &latest))
		latest.Finalizers = nil
		removeErr := k8s.Update(ctx, &latest)
		if removeErr == nil {
			finalizerRemoved = true
			break
		}
		if !apierrors.IsForbidden(removeErr) && !apierrors.IsInvalid(removeErr) {
			t.Fatalf("unexpected error while waiting for valid parameters: %v", removeErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, finalizerRemoved, "finalizer removal should succeed after restoring valid admission parameters")
}
