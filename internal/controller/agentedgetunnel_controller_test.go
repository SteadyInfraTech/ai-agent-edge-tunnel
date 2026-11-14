package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare/fake"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	testHostname = "agent-job-001.agents.example.com"
	testImage    = "example.invalid/cloudflared:test"
)

// harness wires a reconciler to a fake Kubernetes client and a fake Cloudflare.
type harness struct {
	t   *testing.T
	k8s client.Client
	cf  *fake.Client
	r   *AgentEdgeTunnelReconciler
	req ctrl.Request
	now time.Time
}

// newHarness builds a reconciler around one AgentEdgeTunnel. The default object
// is the common case: protected by an email policy and routed to a Service.
// mutate adjusts it before it is stored.
func newHarness(t *testing.T, mutate func(*agentopsv1alpha1.AgentEdgeTunnel), objs ...client.Object) *harness {
	t.Helper()
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme.Scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme.Scheme))

	cr := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent-tunnel", Namespace: "default", UID: "11111111-aaaa-bbbb-cccc-222222222222"},
		Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
			Service:    &agentopsv1alpha1.ServiceTarget{Name: "langgraph-worker", Port: 8080},
			Cloudflare: agentopsv1alpha1.CloudflareConfig{ZoneID: "zone", Subdomain: "agent-job-001"},
			TTL:        "1h",
			Security:   agentopsv1alpha1.SecurityConfig{AllowedEmails: []string{"dev@example.com"}},
		},
	}
	if mutate != nil {
		mutate(cr)
	}
	fixtures := append(serviceTestFixtures(cr.Namespace, "langgraph-worker", "10.96.0.10", "10.244.0.10"), serviceTestFixtures(cr.Namespace, "support-agent", "10.96.0.11", "10.244.0.11")...)
	objects := append([]client.Object{cr}, fixtures...)
	objects = append(objects, objs...)
	k8s := fakeclient.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objects...).
		WithStatusSubresource(cr, &appsv1.Deployment{}, &agentopsv1alpha1.NamespaceTunnelQuota{}).
		Build()
	h := &harness{
		t:   t,
		k8s: k8s,
		cf:  fake.New(),
		req: ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}},
		now: t0,
	}
	h.r = &AgentEdgeTunnelReconciler{
		Client:           k8s,
		APIReader:        k8s,
		Scheme:           scheme.Scheme,
		CloudflareClient: h.cf,
		AccountID:        "fake-account",
		ZoneID:           "zone",
		BaseDomain:       "agents.example.com",
		ConnectorImage:   testImage,
		// The harness uses the in-memory fake, which is what --dry-run runs on.
		DryRun: true,
		Now:    func() time.Time { return h.now },
	}
	return h
}

// serviceTestFixtures builds a selector-backed ClusterIP Service and the
// EndpointSlice the Kubernetes controller would publish for its selected Pod.
func serviceTestFixtures(namespace, name, clusterIP, podIP string) []client.Object {
	podName := name + "-pod"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace, UID: types.UID("uid-" + podName), Labels: map[string]string{"app": name}},
		Status:     corev1.PodStatus{PodIP: podIP, PodIPs: []corev1.PodIP{{IP: podIP}}},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: clusterIP,
			Selector:  map[string]string{"app": name},
			Ports:     []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name + "-slice", Namespace: namespace, Labels: map[string]string{discoveryv1.LabelServiceName: name}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{podIP},
			TargetRef: &corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: namespace, Name: podName, UID: pod.UID},
		}},
	}
	return []client.Object{service, pod, slice}
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

func (h *harness) connectorKey() client.ObjectKey {
	return client.ObjectKey{Namespace: "default", Name: "test-agent-tunnel-cloudflared"}
}

func (h *harness) deployment() (*appsv1.Deployment, error) {
	var d appsv1.Deployment
	err := h.k8s.Get(context.Background(), h.connectorKey(), &d)
	return &d, err
}

func (h *harness) secret() (*corev1.Secret, error) {
	var s corev1.Secret
	err := h.k8s.Get(context.Background(), h.connectorKey(), &s)
	return &s, err
}

func (h *harness) markConnectorReady() {
	h.t.Helper()
	d, err := h.deployment()
	require.NoError(h.t, err)
	d.Status.Replicas, d.Status.ReadyReplicas = 1, 1
	require.NoError(h.t, h.k8s.Status().Update(context.Background(), d))
}

func (h *harness) assertConnectorGone() {
	h.t.Helper()
	_, err := h.deployment()
	assert.True(h.t, apierrors.IsNotFound(err), "connector Deployment should be gone")
	_, err = h.secret()
	assert.True(h.t, apierrors.IsNotFound(err), "connector Secret should be gone")
}

// TestProvisionsProtectedTunnelWithConnector checks: provisions protected tunnel with connector.
func TestProvisionsProtectedTunnelWithConnector(t *testing.T) {
	h := newHarness(t, nil)

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.Equal(t, testHostname, got.Status.Hostname)
	assert.NotEmpty(t, got.Status.TunnelID)
	assert.Contains(t, got.Finalizers, FinalizerName)
	require.NotNil(t, got.Status.ExpiresAt)
	assert.True(t, got.Status.ExpiresAt.Time.Equal(t0.Add(time.Hour)))
	assert.Equal(t, got.Status.TunnelID, h.cf.DNSRecords[testHostname])

	// Access protects the hostname, and its ID is recorded for cleanup.
	require.NotEmpty(t, got.Status.AccessAppID)
	app, ok := h.cf.AccessApps[got.Status.AccessAppID]
	require.True(t, ok)
	assert.Equal(t, testHostname, app.Domain)
	assert.Equal(t, []string{"dev@example.com"}, app.Emails)

	// The connector exists but is not connected yet.
	assert.False(t, got.Status.ConnectorReady)
	assert.Contains(t, got.Status.Message, "waiting for the cloudflared connector")
	dep, err := h.deployment()
	require.NoError(t, err)
	assert.Equal(t, testImage, dep.Spec.Template.Spec.Containers[0].Image)
	assert.True(t, metav1.IsControlledBy(dep, &got), "Deployment must be owned so it is garbage collected")

	// The credentials Secret holds the tunnel secret and an ingress that sends
	// only this hostname to the target Service.
	sec, err := h.secret()
	require.NoError(t, err)
	assert.True(t, metav1.IsControlledBy(sec, &got))
	require.NotNil(t, sec.Immutable)
	assert.True(t, *sec.Immutable, "connector credentials must be immutable")
	assert.Contains(t, string(sec.Data[credentialsKey]), got.Status.TunnelID)
	cfg := string(sec.Data[configKey])
	assert.Contains(t, cfg, "hostname: "+testHostname)
	assert.Contains(t, cfg, "service: http://langgraph-worker.default.svc:8080")
	assert.Contains(t, cfg, "service: http_status:404", "everything else must 404")

	// Once the pod is connected, status follows.
	h.markConnectorReady()
	_, err = h.reconcile()
	require.NoError(t, err)
	got = h.get()
	assert.True(t, got.Status.ConnectorReady)
	assert.Empty(t, got.Status.Message)
}

// TestAccessFailureNeverExposesHostname checks: access failure never exposes hostname.
func TestAccessFailureNeverExposesHostname(t *testing.T) {
	h := newHarness(t, nil)
	h.cf.CreateAccessErr = errors.New("access api down")

	_, err := h.reconcile()
	require.Error(t, err)

	// The protection could not be created, so nothing may be reachable.
	assert.Empty(t, h.cf.DNSRecords, "DNS must not exist without an Access policy")
	_, derr := h.deployment()
	assert.True(t, apierrors.IsNotFound(derr), "connector must not run without an Access policy")
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)

	h.cf.CreateAccessErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "retry must reuse the tunnel")
	assert.NotEmpty(t, h.cf.DNSRecords)
}

// TestRetryAfterDNSFailureDoesNotLeak checks: retry after dns failure does not leak.
func TestRetryAfterDNSFailureDoesNotLeak(t *testing.T) {
	h := newHarness(t, nil)
	h.cf.CreateDNSErr = errors.New("cloudflare is down")

	_, err := h.reconcile()
	require.Error(t, err)
	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.NotEmpty(t, got.Status.TunnelID, "tunnel ID is recorded before DNS is attempted")
	assert.NotEmpty(t, got.Status.AccessAppID, "Access ID is recorded before DNS is attempted")

	h.cf.CreateDNSErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "retry must reuse the tunnel")
	assert.Equal(t, 1, h.cf.CreateAccessCalls, "retry must reuse the Access application")
	assert.Len(t, h.cf.Tunnels, 1)
	assert.Len(t, h.cf.AccessApps, 1)
}

// TestPublicTunnelSkipsAccess checks: public tunnel skips access.
func TestPublicTunnelSkipsAccess(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Spec.Security = agentopsv1alpha1.SecurityConfig{AllowPublic: true}
	})
	h.r.PublicNamespaces = map[string]struct{}{"default": {}}

	_, err := h.reconcile()
	require.NoError(t, err)

	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Zero(t, h.cf.CreateAccessCalls)
	assert.Empty(t, h.get().Status.AccessAppID)
	assert.NotEmpty(t, h.cf.DNSRecords)
}

// TestRejectsUnprotectedAndUnsupportedSecurity checks: rejects unprotected and unsupported security.
func TestRejectsUnprotectedAndUnsupportedSecurity(t *testing.T) {
	for name, sec := range map[string]agentopsv1alpha1.SecurityConfig{
		"no policy at all": {},
		"both emails and allowPublic": {
			AllowedEmails: []string{"dev@example.com"}, AllowPublic: true,
		},
		"webhook validation is not implemented": {
			AllowedEmails:     []string{"dev@example.com"},
			WebhookValidation: &agentopsv1alpha1.WebhookValidation{Provider: "github"},
		},
		"service tokens are not implemented": {
			AllowedEmails: []string{"dev@example.com"}, ServiceTokens: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.Security = sec })

			res, err := h.reconcile()
			require.NoError(t, err, "a spec error is not retried")
			assert.Zero(t, res.RequeueAfter)

			assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
			assert.Zero(t, h.cf.CreateTunnelCalls, "nothing may be created for an invalid spec")
			assert.Empty(t, h.cf.DNSRecords)
		})
	}
}

// TestTunnelWithoutServiceRunsNoConnector checks: tunnel without service runs no connector.
func TestTunnelWithoutServiceRunsNoConnector(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.Service = nil })

	_, err := h.reconcile()
	require.NoError(t, err)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.False(t, got.Status.ConnectorReady)
	assert.Contains(t, got.Status.Message, "no spec.service")
	h.assertConnectorGone()
}

// TestForeignSecretFailsBeforeCreatingATunnel checks: foreign secret fails before creating a tunnel.
func TestForeignSecretFailsBeforeCreatingATunnel(t *testing.T) {
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "test-agent-tunnel-cloudflared", Namespace: "default"}}
	h := newHarness(t, nil, foreign)

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite")

	assert.Zero(t, h.cf.CreateTunnelCalls, "a taken Secret name must fail before any Cloudflare call")
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.get().Status.TunnelID)
	sec, err := h.secret()
	require.NoError(t, err)
	assert.Empty(t, sec.Data, "the foreign Secret must be untouched")
}

// flakySecretCreateClient simulates an API timeout either before or after
// the API server persists a newly-created connector Secret.
type flakySecretCreateClient struct {
	client.Client
	stored bool
	failed bool
}

func (c *flakySecretCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Secret); ok && !c.failed {
		c.failed = true
		if c.stored {
			if err := c.Client.Create(ctx, obj, opts...); err != nil {
				return err
			}
		}
		return errors.New("etcdserver: request timed out")
	}
	return c.Client.Create(ctx, obj, opts...)
}

// TestAmbiguousConnectorSecretWriteRecoversPersistedCredentials verifies
// that a committed Secret and its Cloudflare tunnel survive an ambiguous
// create response and are reused on the next reconcile.
func TestAmbiguousConnectorSecretWriteRecoversPersistedCredentials(t *testing.T) {
	h := newHarness(t, nil)
	h.r.Client = &flakySecretCreateClient{Client: h.k8s, stored: true}

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storing connector credentials")
	assert.Len(t, h.cf.Tunnels, 1, "the tunnel backing the possibly-persisted credentials must be kept")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls)
	assert.Empty(t, h.get().Status.TunnelID)

	secretBefore, err := h.secret()
	require.NoError(t, err)
	require.NotNil(t, secretBefore.Immutable)
	require.True(t, *secretBefore.Immutable)
	dataBefore := map[string][]byte{}
	for key, value := range secretBefore.Data {
		dataBefore[key] = append([]byte(nil), value...)
	}

	h.r.Client = h.k8s
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "the retry must recover rather than create another tunnel")
	secretAfter, err := h.secret()
	require.NoError(t, err)
	assert.Equal(t, dataBefore, secretAfter.Data, "recovery must not rewrite immutable Secret data")
	assert.True(t, *secretAfter.Immutable)
}

// TestAmbiguousConnectorSecretWriteWithoutCommitDoesNotDuplicateTunnel ensures
// a tunnel is not duplicated when the Secret write definitely did not commit.
func TestAmbiguousConnectorSecretWriteWithoutCommitDoesNotDuplicateTunnel(t *testing.T) {
	h := newHarness(t, nil)
	h.r.Client = &flakySecretCreateClient{Client: h.k8s, stored: false}

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Len(t, h.cf.Tunnels, 1, "the tunnel is retained because the write outcome may be ambiguous")
	assert.Empty(t, h.get().Status.TunnelID)

	h.r.Client = h.k8s
	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership cannot be established")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "the unrecoverable orphan must not trigger another create")
	assert.Len(t, h.cf.Tunnels, 1, "the unproven tunnel must remain untouched")
}

// TestMissingConnectorSecretIsReportedNotSilentlyIgnored checks: missing connector secret is reported not silently ignored.
func TestMissingConnectorSecretIsReportedNotSilentlyIgnored(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)

	sec, err := h.secret()
	require.NoError(t, err)
	require.NoError(t, h.k8s.Delete(context.Background(), sec))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete and recreate")
	assert.Contains(t, h.get().Status.Message, "delete and recreate")
	_, deploymentErr := h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "owned Deployment must be removed when credentials are missing")
}

// TestExistingConnectorSecretIsSealed verifies that valid credentials
// created before Secret immutability was introduced are sealed in place.
func TestExistingConnectorSecretIsSealed(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)

	secret, err := h.secret()
	require.NoError(t, err)
	secret.Immutable = nil
	require.NoError(t, h.k8s.Update(context.Background(), secret))

	_, err = h.reconcile()
	require.NoError(t, err)
	sealed, err := h.secret()
	require.NoError(t, err)
	require.NotNil(t, sealed.Immutable)
	assert.True(t, *sealed.Immutable)
}

// TestModifiedConnectorSecretIsRejectedAndConnectorStopped verifies that
// cloudflared never continues with credentials or ingress configuration that
// no longer match the owning AgentEdgeTunnel.
func TestModifiedConnectorSecretIsRejectedAndConnectorStopped(t *testing.T) {
	cases := map[string]func(*corev1.Secret){
		"configuration changed": func(s *corev1.Secret) {
			s.Data[configKey] = []byte("tunnel: attacker\ningress: []\n")
		},
		"credentials changed": func(s *corev1.Secret) {
			s.Data[credentialsKey] = []byte(`{"AccountTag":"fake-account","TunnelSecret":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","TunnelID":"attacker"}`)
		},
		"owner removed": func(s *corev1.Secret) {
			s.OwnerReferences = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			_, err := h.reconcile()
			require.NoError(t, err)
			secret, err := h.secret()
			require.NoError(t, err)
			mutate(secret)
			require.NoError(t, h.k8s.Update(context.Background(), secret))

			_, err = h.reconcile()
			require.Error(t, err)
			assert.Contains(t, h.get().Status.Message, "connector credentials Secret")
			_, deploymentErr := h.deployment()
			assert.True(t, apierrors.IsNotFound(deploymentErr), "connector Deployment must be removed")
			storedSecret, secretErr := h.secret()
			if name == "owner removed" {
				require.NoError(t, secretErr)
				assert.Nil(t, storedSecret.OwnerReferences, "foreign Secret must be left untouched")
			} else {
				assert.True(t, apierrors.IsNotFound(secretErr), "invalid owned Secret must be removed")
			}
		})
	}
}

// TestTTLExpiryCleansEverythingAndDoesNotReprovision checks: ttl expiry cleans everything and does not reprovision.
func TestTTLExpiryCleansEverythingAndDoesNotReprovision(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 1, h.cf.CreateTunnelCalls)

	h.now = t0.Add(2 * time.Hour)
	_, err = h.reconcile()
	require.NoError(t, err)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, got.Status.Phase)
	assert.Empty(t, got.Status.AccessAppID)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.DNSRecords)
	assert.Empty(t, h.cf.AccessApps)
	h.assertConnectorGone()

	// Terminated is final: it must not be provisioned again.
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "terminated tunnel must not be re-created")
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, h.get().Status.Phase)
	h.assertConnectorGone()
}

// TestDeleteCleansEverythingViaFinalizer checks: delete cleans everything via finalizer.
func TestDeleteCleansEverythingViaFinalizer(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Len(t, h.cf.Tunnels, 1)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))

	_, err = h.reconcile()
	require.NoError(t, err)

	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.DNSRecords)
	assert.Empty(t, h.cf.AccessApps)
	h.assertConnectorGone()
	var gone agentopsv1alpha1.AgentEdgeTunnel
	err = h.k8s.Get(context.Background(), h.req.NamespacedName, &gone)
	assert.True(t, apierrors.IsNotFound(err), "finalizer should be removed after cleanup")
}

// TestCleanupFailureKeepsFinalizerAndRetries checks: cleanup failure keeps finalizer and retries.
func TestCleanupFailureKeepsFinalizerAndRetries(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	h.cf.DeleteTunnelErr = errors.New("tunnel still has active connections")

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, h.get().Finalizers, FinalizerName, "must not drop the finalizer while Cloudflare resources remain")
	assert.Len(t, h.cf.Tunnels, 1)
	assert.Len(t, h.cf.AccessApps, 1, "Access must remain until tunnel deletion succeeds")
	// The connector goes first: Cloudflare refuses to delete a tunnel that is
	// still connected, so stopping it is what lets the retry succeed. Access
	// protection is retained while the tunnel still exists.
	h.assertConnectorGone()

	h.cf.DeleteTunnelErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.AccessApps)
}

// TestInvalidTTLFailsWithoutCreatingAnything checks: invalid ttl fails without creating anything.
func TestInvalidTTLFailsWithoutCreatingAnything(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.TTL = "not-a-duration" })

	res, err := h.reconcile()
	require.NoError(t, err, "a spec error is not retried")
	assert.Zero(t, res.RequeueAfter)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "ttl")
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestEmptyTTLMeansNoExpiry checks: empty ttl means no expiry.
func TestEmptyTTLMeansNoExpiry(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.TTL = "" })
	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Nil(t, h.get().Status.ExpiresAt)
}

// TestConnectorPodIsHardened checks: connector pod is hardened.
func TestConnectorPodIsHardened(t *testing.T) {
	cr := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", UID: "u"},
		Spec:       agentopsv1alpha1.AgentEdgeTunnelSpec{Service: &agentopsv1alpha1.ServiceTarget{Name: "svc", Port: 80}},
	}
	r := &AgentEdgeTunnelReconciler{Scheme: scheme.Scheme, ConnectorImage: testImage}
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme.Scheme))

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x-cloudflared", Namespace: "ns"}}
	require.NoError(t, r.mutateConnector(dep, cr))

	ps := dep.Spec.Template.Spec
	require.Len(t, ps.Containers, 1)
	c := ps.Containers[0]

	assert.Equal(t, false, *ps.AutomountServiceAccountToken, "cloudflared never needs the Kubernetes API")
	assert.True(t, *ps.SecurityContext.RunAsNonRoot)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, ps.SecurityContext.SeccompProfile.Type)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)

	// The tunnel secret must reach the pod only through the mounted Secret,
	// never as an argument or environment variable that `kubectl describe`
	// would print.
	assert.Empty(t, c.Env)
	assert.Empty(t, c.EnvFrom)
	for _, a := range c.Args {
		assert.False(t, strings.Contains(strings.ToLower(a), "token") || strings.Contains(strings.ToLower(a), "secret"), "argument %q looks like a credential", a)
	}
	var cfgVol *corev1.Volume
	for i := range ps.Volumes {
		if ps.Volumes[i].Name == "config" {
			cfgVol = &ps.Volumes[i]
		}
	}
	require.NotNil(t, cfgVol)
	assert.Equal(t, int32(0o440), *cfgVol.Secret.DefaultMode)
	assert.Equal(t, "/ready", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, int32(1), *dep.Spec.Replicas)
}

// TestConnectorRefusesToAdoptForeignDeployment checks: connector refuses to adopt foreign deployment.
func TestConnectorRefusesToAdoptForeignDeployment(t *testing.T) {
	cr := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", UID: "u"},
		Spec:       agentopsv1alpha1.AgentEdgeTunnelSpec{Service: &agentopsv1alpha1.ServiceTarget{Name: "svc", Port: 80}},
	}
	require.NoError(t, agentopsv1alpha1.AddToScheme(scheme.Scheme))
	r := &AgentEdgeTunnelReconciler{Scheme: scheme.Scheme, ConnectorImage: testImage}

	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x-cloudflared", Namespace: "ns", ResourceVersion: "7"}}
	err := r.mutateConnector(foreign, cr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to adopt")
}

// TestConnectorConfigRendering checks: connector config rendering.
func TestConnectorConfigRendering(t *testing.T) {
	got := connectorConfig("tid", "a.example.com", &agentopsv1alpha1.ServiceTarget{Name: "svc", Port: 9000}, "team-a")
	assert.Equal(t, `tunnel: tid
credentials-file: /etc/cloudflared/credentials.json
ingress:
  - hostname: a.example.com
    service: http://svc.team-a.svc:9000
  - service: http_status:404
`, got)
}

// TestTunnelNameIsUniquePerNamespace checks: tunnel name is unique per namespace.
func TestTunnelNameIsUniquePerNamespace(t *testing.T) {
	a := &agentopsv1alpha1.AgentEdgeTunnel{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a", UID: "aaaaaaaa-1111"}}
	b := &agentopsv1alpha1.AgentEdgeTunnel{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-b", UID: "bbbbbbbb-2222"}}
	assert.NotEqual(t, tunnelName(a), tunnelName(b))
}

// TestEmptyCloudflareSettingsFailBeforeCreatingAnything checks: empty cloudflare settings fail before creating anything.
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

// TestRealModeDiscardsStatusWrittenByDryRun checks: real mode discards status written by dry run.
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
	h.assertConnectorGone()
}

// TestRealModePreservesForeignConnectorSecret verifies mode switching removes
// the owned Deployment but never deletes a Secret whose ownership was changed.
func TestRealModePreservesForeignConnectorSecret(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)

	secret, err := h.secret()
	require.NoError(t, err)
	originalData := map[string][]byte{}
	for key, value := range secret.Data {
		originalData[key] = append([]byte(nil), value...)
	}
	secret.OwnerReferences = nil
	require.NoError(t, h.k8s.Update(context.Background(), secret))

	h.r.DryRun = false
	res, err := h.reconcile()
	require.NoError(t, err)
	assert.True(t, res.Requeue)
	assert.Empty(t, h.get().Status.TunnelID)

	_, deploymentErr := h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "owned connector Deployment should be removed")
	preserved, secretErr := h.secret()
	require.NoError(t, secretErr)
	assert.Empty(t, preserved.OwnerReferences, "foreign Secret ownership must remain untouched")
	assert.Equal(t, originalData, preserved.Data, "foreign Secret data must remain untouched")
}

// TestRealModeRetainsFakeStatusWhenConnectorCleanupFails ensures a failed
// resource deletion cannot advance the mode switch or discard its retry state.
func TestRealModeRetainsFakeStatusWhenConnectorCleanupFails(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	fakeID := h.get().Status.TunnelID

	h.r.DryRun = false
	h.r.Client = &failingDeploymentDeleteClient{Client: h.k8s, err: errors.New("deployment deletion blocked")}
	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "removing dry-run connector resources")
	assert.Equal(t, fakeID, h.get().Status.TunnelID, "fake status must remain so cleanup can be retried")
	_, secretErr := h.secret()
	assert.NoError(t, secretErr, "the Secret must not be deleted after Deployment cleanup fails")

	h.r.Client = h.k8s
	res, err := h.reconcile()
	require.NoError(t, err)
	assert.True(t, res.Requeue)
	assert.Empty(t, h.get().Status.TunnelID)
	h.assertConnectorGone()
}

// TestRealModeDeletesDryRunObjectWithoutPretendingToCleanUp checks: real mode deletes dry run object without pretending to clean up.
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

// TestDryRunRefusesObjectsWithRealIDsAndKeepsFinalizer checks: dry run refuses objects with real i ds and keeps finalizer.
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
	// onAccess fails the write that records the Access application instead of
	// the one that records the tunnel.
	onAccess bool
}

// Status returns a writer whose Update can fail on demand.
func (c *flakyStatusClient) Status() client.SubResourceWriter {
	return &flakyStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type flakyStatusWriter struct {
	client.SubResourceWriter
	c *flakyStatusClient
}

// Update fails the status write that records a new tunnel, optionally after
// applying it.
func (w *flakyStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	t, ok := obj.(*agentopsv1alpha1.AgentEdgeTunnel)
	hit := ok && t.Status.Phase == agentopsv1alpha1.PhasePending &&
		((w.c.onAccess && t.Status.AccessAppID != "") ||
			(!w.c.onAccess && t.Status.TunnelID != "" && t.Status.AccessAppID == ""))
	if hit && !w.c.failed {
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

// TestAmbiguousStatusWriteKeepsTheTunnel checks: ambiguous status write keeps the tunnel.
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
			} else {
				// The write was lost: the tunnel is found again by its name and adopted.
				assert.Empty(t, h.get().Status.TunnelID)
				_, err = h.reconcile()
				require.NoError(t, err)
				assert.Equal(t, 1, h.cf.CreateTunnelCalls, "the retry must adopt the orphan, not create a second tunnel")
				require.Len(t, h.cf.Tunnels, 1)
				for id := range h.cf.Tunnels {
					assert.Equal(t, id, h.get().Status.TunnelID)
				}
				assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
			}
		})
	}
}

// TestRejectsInvalidServiceTarget checks: rejects invalid service target.
func TestRejectsInvalidServiceTarget(t *testing.T) {
	for name, svc := range map[string]*agentopsv1alpha1.ServiceTarget{
		"name with a newline": {Name: "svc\n  - hostname: evil.example.com", Port: 8080},
		"name with a slash":   {Name: "svc/evil", Port: 8080},
		"port zero":           {Name: "svc", Port: 0},
		"port too large":      {Name: "svc", Port: 70000},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.Service = svc })

			res, err := h.reconcile()
			require.NoError(t, err, "a spec error is not retried")
			assert.Zero(t, res.RequeueAfter)
			assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
			assert.Zero(t, h.cf.CreateTunnelCalls, "nothing may be created for an invalid spec")
		})
	}
}

// TestRejectsUnsafeServiceTargetsBeforeProvisioning ensures a tunnel cannot
// route through an ExternalName, headless, selectorless, or manually redirected
// Service, or to a Pod outside the Service's own selector.
func TestRejectsUnsafeServiceTargetsBeforeProvisioning(t *testing.T) {
	cases := map[string]func(*harness){
		"missing Service": func(h *harness) {
			var service corev1.Service
			require.NoError(h.t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "langgraph-worker"}, &service))
			require.NoError(h.t, h.k8s.Delete(context.Background(), &service))
		},
		"ExternalName Service": func(h *harness) {
			service := getTestService(h)
			service.Spec.Type = corev1.ServiceTypeExternalName
			service.Spec.ExternalName = "169.254.169.254"
			require.NoError(h.t, h.k8s.Update(context.Background(), service))
		},
		"headless Service": func(h *harness) {
			service := getTestService(h)
			service.Spec.ClusterIP = corev1.ClusterIPNone
			require.NoError(h.t, h.k8s.Update(context.Background(), service))
		},
		"selectorless Service": func(h *harness) {
			service := getTestService(h)
			service.Spec.Selector = nil
			require.NoError(h.t, h.k8s.Update(context.Background(), service))
		},
		"Service with externalIPs": func(h *harness) {
			service := getTestService(h)
			service.Spec.ExternalIPs = []string{"192.0.2.10"}
			require.NoError(h.t, h.k8s.Update(context.Background(), service))
		},
		"requested port is not exposed": func(h *harness) {
			cr := h.get()
			cr.Spec.Service.Port = 9090
			require.NoError(h.t, h.k8s.Update(context.Background(), &cr))
		},
		"EndpointSlice has no Pod reference": func(h *harness) {
			slice := getTestEndpointSlice(h)
			slice.Endpoints[0].TargetRef = nil
			require.NoError(h.t, h.k8s.Update(context.Background(), slice))
		},
		"EndpointSlice references another namespace": func(h *harness) {
			slice := getTestEndpointSlice(h)
			slice.Endpoints[0].TargetRef.Namespace = "kube-system"
			require.NoError(h.t, h.k8s.Update(context.Background(), slice))
		},
		"selected Pod uses host networking": func(h *harness) {
			pod := getTestPod(h)
			pod.Spec.HostNetwork = true
			require.NoError(h.t, h.k8s.Update(context.Background(), pod))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			mutate(h)
			_, err := h.reconcile()
			require.NoError(t, err, "an unsafe Service target is recorded as a policy failure")
			assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
			assert.Contains(t, h.get().Status.Message, errUnsafeServiceTarget.Error())
			assert.Zero(t, h.cf.CreateTunnelCalls, "Cloudflare resources must not be created for an unsafe origin")
		})
	}
}

// TestProvisioningRetriesTransientServiceEndpointMismatch ensures a tunnel
// without a recorded Cloudflare ID remains Pending and rechecks without waiting
// for another Kubernetes event.
func TestProvisioningRetriesTransientServiceEndpointMismatch(t *testing.T) {
	h := newHarness(t, nil)
	slice := getTestEndpointSlice(h)
	slice.Endpoints[0].Addresses = []string{"192.0.2.10"}
	require.NoError(t, h.k8s.Update(context.Background(), slice))

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhasePending, h.get().Status.Phase)
	assert.Equal(t, serviceTargetRetryInterval, res.RequeueAfter)
	assert.NotNil(t, h.get().Status.ServiceTargetVerificationSince)
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestPendingTunnelWithRecordedIDRetriesTransientServiceEndpointMismatch
// ensures provisioning does not destroy an already-created tunnel during a
// short Pod/EndpointSlice rollout race.
func TestPendingTunnelWithRecordedIDRetriesTransientServiceEndpointMismatch(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	active := h.get()
	active.Status.Phase = agentopsv1alpha1.PhasePending
	require.NoError(t, h.k8s.Status().Update(context.Background(), &active))

	slice := getTestEndpointSlice(h)
	slice.Endpoints[0].Addresses = []string{"192.0.2.10"}
	require.NoError(t, h.k8s.Update(context.Background(), slice))

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhasePending, h.get().Status.Phase)
	assert.Equal(t, serviceTargetRetryInterval, res.RequeueAfter)
	assert.Equal(t, active.Status.TunnelID, h.get().Status.TunnelID)
	assert.Contains(t, h.cf.Tunnels, active.Status.TunnelID)
	assert.Equal(t, active.Status.TunnelID, h.cf.DNSRecords[active.Status.Hostname])

	slice = getTestEndpointSlice(h)
	slice.Endpoints[0].Addresses = []string{"10.244.0.10"}
	require.NoError(t, h.k8s.Update(context.Background(), slice))
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Nil(t, h.get().Status.ServiceTargetVerificationSince)
	assert.Equal(t, active.Status.TunnelID, h.get().Status.TunnelID)
	assert.Len(t, h.cf.Tunnels, 1)
}

// TestActiveTunnelTerminatesWhenServiceEndpointRemainsUnsafe ensures an
// EndpointSlice mismatch is tolerated for a time-bounded grace period, then
// removes an already-exposed tunnel if the mismatch persists. Immediate
// reconciles model status-update/watch events and must not consume that period.
func TestActiveTunnelTerminatesWhenServiceEndpointRemainsUnsafe(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	require.Len(t, h.cf.Tunnels, 1)
	require.NotEmpty(t, h.cf.DNSRecords)

	slice := getTestEndpointSlice(h)
	slice.Endpoints[0].Addresses = []string{"192.0.2.10"}
	require.NoError(t, h.k8s.Update(context.Background(), slice))

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, serviceTargetRetryInterval, res.RequeueAfter)
	startedAt := h.get().Status.ServiceTargetVerificationSince
	require.NotNil(t, startedAt)
	require.True(t, startedAt.Time.Equal(h.now), "verification start should use the reconciler clock")
	require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)

	// The status update above would enqueue another reconcile through the
	// controller's primary watch. Repeated immediate reconciles must not
	// consume the grace period or terminate a healthy tunnel during a rollout.
	for i := 0; i < 8; i++ {
		res, err = h.reconcile()
		require.NoError(t, err)
		assert.Equal(t, serviceTargetRetryInterval, res.RequeueAfter)
		assert.Equal(t, startedAt, h.get().Status.ServiceTargetVerificationSince)
		assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
		assert.Len(t, h.cf.Tunnels, 1)
		assert.NotEmpty(t, h.cf.DNSRecords)
	}

	h.now = h.now.Add(serviceTargetGracePeriod + time.Second)
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, h.get().Status.Phase)
	assert.Contains(t, h.get().Status.Message, "service target became unsafe")
	assert.Empty(t, h.cf.DNSRecords)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.AccessApps)
	h.assertConnectorGone()
}

// TestActiveTunnelSurvivesTransientEndpointSliceConvergence ensures a
// rollout-related Pod/EndpointSlice mismatch is retried and recovers in place.
func TestActiveTunnelSurvivesTransientEndpointSliceConvergence(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	active := h.get()
	slice := getTestEndpointSlice(h)
	slice.Endpoints[0].Addresses = []string{"192.0.2.10"}
	require.NoError(t, h.k8s.Update(context.Background(), slice))

	res, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, serviceTargetRetryInterval, res.RequeueAfter)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Contains(t, h.cf.Tunnels, active.Status.TunnelID)

	slice = getTestEndpointSlice(h)
	slice.Endpoints[0].Addresses = []string{"10.244.0.10"}
	require.NoError(t, h.k8s.Update(context.Background(), slice))
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Nil(t, h.get().Status.ServiceTargetVerificationSince)
	assert.Contains(t, h.cf.Tunnels, active.Status.TunnelID)
	assert.Equal(t, active.Status.TunnelID, h.cf.DNSRecords[active.Status.Hostname])
}

func getTestService(h *harness) *corev1.Service {
	h.t.Helper()
	var service corev1.Service
	require.NoError(h.t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "langgraph-worker"}, &service))
	return &service
}

func getTestPod(h *harness) *corev1.Pod {
	h.t.Helper()
	var pod corev1.Pod
	require.NoError(h.t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "langgraph-worker-pod"}, &pod))
	return &pod
}

func getTestEndpointSlice(h *harness) *discoveryv1.EndpointSlice {
	h.t.Helper()
	var slice discoveryv1.EndpointSlice
	require.NoError(h.t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "langgraph-worker-slice"}, &slice))
	return &slice
}

// TestAmbiguousAccessStatusWriteKeepsTheApplication checks: a failed status
// write after creating the Access application must not delete it.
func TestAmbiguousAccessStatusWriteKeepsTheApplication(t *testing.T) {
	h := newHarness(t, nil)
	h.r.Client = &flakyStatusClient{Client: h.k8s, stored: true, onAccess: true}

	_, err := h.reconcile()
	require.Error(t, err)

	require.Len(t, h.cf.AccessApps, 1, "the application must be kept")
	got := h.get()
	require.NotEmpty(t, got.Status.AccessAppID)
	for id := range h.cf.AccessApps {
		assert.Equal(t, id, got.Status.AccessAppID, "status must name a live application")
	}

	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Equal(t, 1, h.cf.CreateAccessCalls, "the retry must reuse the stored application")
}

// orphanTunnel runs one reconcile whose status write is lost, leaving a tunnel
// and its connector Secret in place with no ID recorded, as after a crash.
func orphanTunnel(t *testing.T, mutate func(*agentopsv1alpha1.AgentEdgeTunnel)) *harness {
	t.Helper()
	h := newHarness(t, mutate)
	real := h.r.Client
	h.r.Client = &flakyStatusClient{Client: h.k8s, stored: false}
	_, err := h.reconcile()
	require.Error(t, err)
	h.r.Client = real
	require.Empty(t, h.get().Status.TunnelID)
	require.Len(t, h.cf.Tunnels, 1)
	return h
}

// TestPersistedImmutableSecretPreventsDuplicateTunnelWhenLookupMisses ensures
// a transiently invisible tunnel does not cause another tunnel to be created
// or the immutable credentials to be overwritten.
func TestPersistedImmutableSecretPreventsDuplicateTunnelWhenLookupMisses(t *testing.T) {
	h := orphanTunnel(t, nil)
	secretBefore, err := h.secret()
	require.NoError(t, err)
	require.NotNil(t, secretBefore.Immutable)
	require.True(t, *secretBefore.Immutable)
	dataBefore := map[string][]byte{}
	for key, value := range secretBefore.Data {
		dataBefore[key] = append([]byte(nil), value...)
	}
	for id := range h.cf.Tunnels {
		delete(h.cf.Tunnels, id) // simulate a temporarily incomplete Cloudflare lookup
	}

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no matching Cloudflare tunnel can be recovered")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "an existing immutable Secret must prevent creating a second tunnel")
	secretAfter, getErr := h.secret()
	require.NoError(t, getErr)
	assert.Equal(t, dataBefore, secretAfter.Data, "persisted credentials must remain unchanged")
	assert.True(t, *secretAfter.Immutable)
}

// TestDeletingAnOrphanTunnelRecoversAndCleansItUp covers deletion after
// Cloudflare created a tunnel and the status write that records its ID was lost.
func TestDeletingAnOrphanTunnelRecoversAndCleansItUp(t *testing.T) {
	h := orphanTunnel(t, nil)
	require.Len(t, h.cf.Tunnels, 1)
	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))

	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Empty(t, h.cf.Tunnels, "the recovered tunnel must be deleted")
	assert.Empty(t, h.cf.DNSRecords)
	assert.Empty(t, h.cf.AccessApps)
	h.assertConnectorGone()
}

// TestDeletingAnOrphanDryRunTunnelInRealModeLeavesItUntouched covers the
// nil recovery result for a fake tunnel when the operator switches to real mode.
func TestDeletingAnOrphanDryRunTunnelInRealModeLeavesItUntouched(t *testing.T) {
	h := orphanTunnel(t, nil)
	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	h.r.DryRun = false

	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Len(t, h.cf.Tunnels, 1, "real mode must not delete a dry-run tunnel")
	var gone agentopsv1alpha1.AgentEdgeTunnel
	assert.True(t, apierrors.IsNotFound(h.k8s.Get(context.Background(), h.req.NamespacedName, &gone)))
}

// TestDeletingAnOrphanRealTunnelInDryRunModeKeepsTheFinalizer covers the
// inverse mode guard: a dry-run operator must not adopt or delete a real ID.
func TestDeletingAnOrphanRealTunnelInDryRunModeKeepsTheFinalizer(t *testing.T) {
	h := orphanTunnel(t, nil)
	const realID = "real-tunnel-12345678"
	var oldID string
	for id, info := range h.cf.Tunnels {
		oldID = id
		delete(h.cf.Tunnels, id)
		info.ID = realID
		h.cf.Tunnels[realID] = info
		break
	}
	require.NotEmpty(t, oldID)
	sec, err := h.secret()
	require.NoError(t, err)
	sec.Data[credentialsKey] = []byte(strings.ReplaceAll(string(sec.Data[credentialsKey]), oldID, realID))
	require.NoError(t, h.k8s.Update(context.Background(), sec))

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to recover a real Cloudflare tunnel in dry-run mode")
	assert.Contains(t, h.cf.Tunnels, realID, "dry-run mode must leave the real tunnel untouched")
	assert.Contains(t, h.get().Finalizers, FinalizerName)
}

// TestDeletingAnOrphanTunnelWithoutOwnershipEvidenceKeepsTheFinalizer ensures
// deletion never removes a same-name tunnel when its controlled Secret cannot
// prove that the tunnel belongs to this object.
func TestDeletingAnOrphanTunnelWithoutOwnershipEvidenceKeepsTheFinalizer(t *testing.T) {
	h := orphanTunnel(t, nil)
	sec, err := h.secret()
	require.NoError(t, err)
	sec.Data[credentialsKey] = []byte(`{"AccountTag":"another-account","TunnelSecret":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","TunnelID":"not-the-orphan"}`)
	require.NoError(t, h.k8s.Update(context.Background(), sec))
	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership cannot be established")
	assert.Len(t, h.cf.Tunnels, 1, "an unproven tunnel must remain untouched")
	assert.Contains(t, h.get().Finalizers, FinalizerName)
}

// TestOrphanTunnelWithoutAServiceRequiresOwnershipEvidence checks that a
// same-name tunnel cannot be adopted without independent ownership evidence.
func TestOrphanTunnelWithoutAServiceRequiresOwnershipEvidence(t *testing.T) {
	h := orphanTunnel(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.Service = nil })
	_, err := h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership cannot be established")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls)
	assert.Len(t, h.cf.Tunnels, 1, "the candidate must remain untouched")
	assert.Empty(t, h.get().Status.TunnelID)
}

// TestOrphanTunnelWithoutItsSecretIsLeftUntouched checks that a missing
// connector Secret is not treated as proof that a same-name tunnel is ours.
func TestOrphanTunnelWithoutItsSecretIsLeftUntouched(t *testing.T) {
	h := orphanTunnel(t, nil)
	var oldID string
	for id := range h.cf.Tunnels {
		oldID = id
	}
	sec, err := h.secret()
	require.NoError(t, err)
	require.NoError(t, h.k8s.Delete(context.Background(), sec))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership cannot be established")
	assert.Contains(t, h.cf.Tunnels, oldID, "the candidate must not be deleted")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "do not create a duplicate while ownership is ambiguous")
	assert.Empty(t, h.get().Status.TunnelID)
}

// TestOrphanTunnelWithIncompleteCredentialsIsLeftUntouched checks that a
// matching tunnel ID is insufficient when the credentials are incomplete.
func TestOrphanTunnelWithIncompleteCredentialsIsLeftUntouched(t *testing.T) {
	for _, credentials := range []string{
		`{"AccountTag":"fake-account","TunnelID":"matching-tunnel"}`,
		`{"AccountTag":"fake-account","TunnelSecret":"","TunnelID":"matching-tunnel"}`,
		`{"AccountTag":"fake-account","TunnelSecret":"not-base64","TunnelID":"matching-tunnel"}`,
		`{"AccountTag":"another-account","TunnelSecret":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","TunnelID":"matching-tunnel"}`,
	} {
		t.Run(credentials, func(t *testing.T) {
			h := orphanTunnel(t, nil)
			var oldID string
			for id := range h.cf.Tunnels {
				oldID = id
			}
			sec, err := h.secret()
			require.NoError(t, err)
			sec.Data[credentialsKey] = []byte(strings.ReplaceAll(credentials, "matching-tunnel", oldID))
			require.NoError(t, h.k8s.Update(context.Background(), sec))

			_, err = h.reconcile()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ownership cannot be established")
			assert.Contains(t, h.cf.Tunnels, oldID, "the candidate must not be deleted")
			assert.Equal(t, 1, h.cf.CreateTunnelCalls, "do not create a duplicate while ownership is ambiguous")
			assert.Empty(t, h.get().Status.TunnelID)
		})
	}
}

// TestOrphanTunnelWhoseSecretNamesAnotherTunnelIsLeftUntouched checks that
// mismatched credentials do not authorize deletion of the candidate tunnel.
func TestOrphanTunnelWhoseSecretNamesAnotherTunnelIsLeftUntouched(t *testing.T) {
	h := orphanTunnel(t, nil)
	var oldID string
	for id := range h.cf.Tunnels {
		oldID = id
	}
	sec, err := h.secret()
	require.NoError(t, err)
	sec.Data[credentialsKey] = []byte(`{"TunnelID":"some-other-tunnel"}`)
	require.NoError(t, h.k8s.Update(context.Background(), sec))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership cannot be established")
	assert.Contains(t, h.cf.Tunnels, oldID)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls)
	assert.Empty(t, h.get().Status.TunnelID)
}

// TestOrphanTunnelWithUncontrolledMatchingSecretIsLeftUntouched verifies that
// matching credentials do not establish ownership when the Secret is not
// controlled by this AgentEdgeTunnel.
func TestOrphanTunnelWithUncontrolledMatchingSecretIsLeftUntouched(t *testing.T) {
	h := orphanTunnel(t, nil)
	var oldID string
	for id := range h.cf.Tunnels {
		oldID = id
	}
	sec, err := h.secret()
	require.NoError(t, err)
	sec.OwnerReferences = nil
	require.NoError(t, h.k8s.Update(context.Background(), sec))

	// Exercise credentialsMatch directly: reconcile rejects this unmanaged
	// Secret earlier in checkConnectorSecretFree.
	tunnel := h.get()
	ok, err := h.r.credentialsMatch(context.Background(), &tunnel, oldID)
	require.NoError(t, err)
	assert.False(t, ok, "matching credentials in an uncontrolled Secret are not ownership evidence")

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by this AgentEdgeTunnel")
	assert.Contains(t, h.cf.Tunnels, oldID, "the candidate tunnel must remain untouched")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls, "do not create a duplicate while the connector Secret is unmanaged")
	assert.Empty(t, h.get().Status.TunnelID)
}

// TestTunnelNamedForAnotherObjectIsNeverTouched checks that only the tunnel named for this object's UID is recovered.
func TestTunnelNamedForAnotherObjectIsNeverTouched(t *testing.T) {
	h := newHarness(t, nil)
	h.cf.Tunnels["foreign"] = &cf.TunnelInfo{ID: "foreign", Name: "agent-default-test-agent-tunnel-99999999"}

	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Contains(t, h.cf.Tunnels, "foreign", "a tunnel for a different UID must be left alone")
	assert.Equal(t, 1, h.cf.CreateTunnelCalls)
	assert.NotEqual(t, "foreign", h.get().Status.TunnelID)
}

// TestFailedTunnelLookupCreatesNothing checks that a lookup error stops provisioning before any tunnel is created.
func TestFailedTunnelLookupCreatesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.cf.FindTunnelsErr = errors.New("cloudflare unavailable")

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Zero(t, h.cf.CreateTunnelCalls, "without knowing whether one exists, creating another could duplicate it")
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
}

// seedAccessApp records an Access application named as the controller names its own.
func seedAccessApp(h *harness, id string, emails []string) {
	h.cf.AccessApps[id] = fake.AccessApp{Name: tunnelName(&agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent-tunnel", Namespace: "default", UID: "11111111-aaaa-bbbb-cccc-222222222222"},
	}), Domain: testHostname, Emails: emails}
}

// TestOrphanAccessAppIsLeftUntouchedWithoutOwnershipProof checks that a
// name/domain match and a matching allow policy do not prove application ownership.
func TestOrphanAccessAppIsLeftUntouchedWithoutOwnershipProof(t *testing.T) {
	h := newHarness(t, nil)
	seedAccessApp(h, "orphan-app", []string{"dev@example.com"})

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership and policy identity cannot be established")
	assert.Zero(t, h.cf.CreateAccessCalls)
	assert.Contains(t, h.cf.AccessApps, "orphan-app", "the candidate must remain untouched")
	assert.Empty(t, h.get().Status.AccessAppID)
}

// TestOrphanAccessAppWithoutPolicyIsLeftUntouched checks that even an
// incomplete same-name application is not deleted without ownership proof.
func TestOrphanAccessAppWithoutPolicyIsLeftUntouched(t *testing.T) {
	h := newHarness(t, nil)
	seedAccessApp(h, "incomplete-app", nil)

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership and policy identity cannot be established")
	assert.Contains(t, h.cf.AccessApps, "incomplete-app")
	assert.Zero(t, h.cf.CreateAccessCalls)
	assert.Empty(t, h.get().Status.AccessAppID)
}

// TestAccessAppOfAnotherNameIsNeverTouched checks that an application not named for this object is left alone.
func TestAccessAppOfAnotherNameIsNeverTouched(t *testing.T) {
	h := newHarness(t, nil)
	h.cf.AccessApps["foreign-app"] = fake.AccessApp{Name: "someone-elses", Domain: testHostname, Emails: nil}

	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Contains(t, h.cf.AccessApps, "foreign-app")
	assert.Equal(t, 1, h.cf.CreateAccessCalls)
}

// TestFailedAccessLookupNeverExposesTheHostname checks that if the lookup fails, no DNS record is created.
func TestFailedAccessLookupNeverExposesTheHostname(t *testing.T) {
	h := newHarness(t, nil)
	h.cf.FindAccessErr = errors.New("cloudflare unavailable")

	_, err := h.reconcile()
	require.Error(t, err)
	assert.Empty(t, h.cf.DNSRecords, "the hostname must not go live without its Access application")
	assert.Zero(t, h.cf.CreateAccessCalls)
}

// TestOneFailingCleanupStepDoesNotBlockTheOthers checks that a DNS failure still lets the tunnel and Access application be removed.
func TestOneFailingCleanupStepDoesNotBlockTheOthers(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Len(t, h.cf.Tunnels, 1)
	require.Len(t, h.cf.AccessApps, 1)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	h.cf.DeleteDNSErr = errors.New("zone not found")

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deleting DNS record")
	assert.Empty(t, h.cf.Tunnels, "the tunnel must not stay in Cloudflare because DNS cleanup failed")
	assert.Empty(t, h.cf.AccessApps)
	assert.Contains(t, h.get().Finalizers, FinalizerName, "the DNS record is still there, so the finalizer must stay")

	h.cf.DeleteDNSErr = nil
	_, err = h.reconcile()
	require.NoError(t, err, "the retry must still find the IDs and finish")
	assert.Empty(t, h.cf.DNSRecords)
}

// failingDeploymentDeleteClient injects a connector Deployment deletion failure.
type failingDeploymentDeleteClient struct {
	client.Client
	err error
}

func (c *failingDeploymentDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*appsv1.Deployment); ok {
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// replacingObjectOnDeleteClient simulates deletion/recreation after the
// controller's ownership read but before its Delete request.
type replacingObjectOnDeleteClient struct {
	client.Client
	kind string
}

func (c *replacingObjectOnDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	matches := false
	switch c.kind {
	case "Deployment":
		_, matches = obj.(*appsv1.Deployment)
	case "Secret":
		_, matches = obj.(*corev1.Secret)
	}
	if !matches {
		return c.Client.Delete(ctx, obj, opts...)
	}
	current := obj.DeepCopyObject().(client.Object)
	if err := c.Client.Delete(ctx, current); err != nil {
		return err
	}
	replacement := obj.DeepCopyObject().(client.Object)
	replacement.SetUID(types.UID("replacement-uid"))
	replacement.SetResourceVersion("")
	replacement.SetOwnerReferences(nil)
	if err := c.Client.Create(ctx, replacement); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// TestDeleteConnectorUsesPreconditions ensures a resource replaced after the
// ownership check is not deleted using the stale object.
func TestDeleteConnectorUsesPreconditions(t *testing.T) {
	for _, kind := range []string{"Deployment", "Secret"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			_, err := h.reconcile()
			require.NoError(t, err)
			var obj client.Object
			if kind == "Deployment" {
				obj, err = h.deployment()
			} else {
				dep, getErr := h.deployment()
				require.NoError(t, getErr)
				require.NoError(t, h.k8s.Delete(context.Background(), dep))
				obj, err = h.secret()
			}
			require.NoError(t, err)
			obj.SetUID(types.UID("owned-" + strings.ToLower(kind)))
			require.NoError(t, h.k8s.Update(context.Background(), obj))

			h.r.Client = &replacingObjectOnDeleteClient{Client: h.k8s, kind: kind}
			tunnel := h.get()
			var rejectedSecret *corev1.Secret
			if kind == "Secret" {
				rejectedSecret = obj.(*corev1.Secret)
			}
			err = h.r.deleteConnector(context.Background(), &tunnel, rejectedSecret, true)
			require.Error(t, err)
			assert.True(t, apierrors.IsConflict(err), "stale delete should conflict, got: %v", err)

			var replacement client.Object
			if kind == "Deployment" {
				replacement = &appsv1.Deployment{}
			} else {
				replacement = &corev1.Secret{}
			}
			require.NoError(t, h.k8s.Get(context.Background(), h.connectorKey(), replacement))
			assert.Equal(t, types.UID("replacement-uid"), replacement.GetUID())
			assert.Empty(t, replacement.GetOwnerReferences(), "replacement resource must remain foreign")
		})
	}
}

// restoringSecretOnNotFoundClient simulates a Secret being recreated after a
// cached read reports NotFound.
type restoringSecretOnNotFoundClient struct {
	client.Client
	restored *corev1.Secret
	done     bool
}

func (c *restoringSecretOnNotFoundClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*corev1.Secret); ok && apierrors.IsNotFound(err) && !c.done {
		c.done = true
		restored := c.restored.DeepCopy()
		restored.ResourceVersion = ""
		restored.UID = types.UID("restored-secret-uid")
		if createErr := c.Client.Create(ctx, restored); createErr != nil {
			return createErr
		}
	}
	return err
}

func TestMissingConnectorSecretDoesNotDeleteSubsequentlyRestoredSecret(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	secret, err := h.secret()
	require.NoError(t, err)
	require.NoError(t, h.k8s.Delete(context.Background(), secret))

	h.r.Client = &restoringSecretOnNotFoundClient{Client: h.k8s, restored: secret}
	err = h.r.validateConnectorSecret(context.Background(), func() *agentopsv1alpha1.AgentEdgeTunnel {
		tunnel := h.get()
		return &tunnel
	}())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is missing")

	_, deploymentErr := h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "owned Deployment must be removed")
	restored, getErr := h.secret()
	require.NoError(t, getErr, "a Secret restored after the missing read must not be deleted")
	assert.Equal(t, types.UID("restored-secret-uid"), restored.UID)
}

func TestAccessBackoffStillValidatesAndCleansInvalidConnectorSecret(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	active := h.get()
	require.NotNil(t, active.Status.AccessPolicyNextCheckAt)

	// Trigger a Cloudflare 429 and enter the verification backoff.
	h.now = active.Status.AccessPolicyNextCheckAt.Time
	h.cf.FindAccessErr = &cf.HTTPStatusError{StatusCode: 429, Message: "Cloudflare API rate limit"}
	_, err = h.reconcile()
	require.NoError(t, err)
	got := h.get()
	require.Contains(t, got.Status.Message, "rate-limited")
	verifyCalls := h.cf.VerifyAccessCalls

	// A lost owner reference makes the persisted Secret invalid while Access
	// verification is deferred. Cleanup must stop the Deployment but preserve
	// the now-foreign Secret, without calling Cloudflare again.
	secret, err := h.secret()
	require.NoError(t, err)
	secret.OwnerReferences = nil
	require.NoError(t, h.k8s.Update(context.Background(), secret))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed")
	assert.Equal(t, verifyCalls, h.cf.VerifyAccessCalls, "backoff must still defer Access verification")
	_, deploymentErr := h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "invalid connector Deployment must be removed during backoff")
	stored, secretErr := h.secret()
	require.NoError(t, secretErr)
	assert.Nil(t, stored.OwnerReferences, "foreign Secret must remain untouched")
}

// TestConnectorRemovalFailureRetainsAccessApplication verifies that an Access
// application remains in place while the connector prevents tunnel deletion.

// TestDeferredAccessSetupStillValidatesConnectorSecret verifies that an Active
// legacy tunnel waiting for initial Access setup still validates credentials
// before returning early, without restoring its Deployment.
func TestDeferredAccessSetupStillValidatesConnectorSecret(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)

	active := h.get()
	active.Status.AccessAppID = ""
	nextCheck := metav1.NewTime(h.now.Add(time.Minute))
	active.Status.AccessPolicyNextCheckAt = &nextCheck
	require.NoError(t, h.k8s.Status().Update(context.Background(), &active))

	secret, err := h.secret()
	require.NoError(t, err)
	secret.OwnerReferences = nil
	require.NoError(t, h.k8s.Update(context.Background(), secret))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed")
	assert.Zero(t, h.cf.VerifyAccessCalls, "deferred initial setup must not call Cloudflare")
	_, deploymentErr := h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "invalid connector Deployment must be removed")
	stored, secretErr := h.secret()
	require.NoError(t, secretErr)
	assert.Nil(t, stored.OwnerReferences, "foreign Secret must remain untouched")
}

func TestConnectorRemovalFailureRetainsAccessApplication(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Len(t, h.cf.Tunnels, 1)
	require.Len(t, h.cf.AccessApps, 1)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	h.r.Client = &failingDeploymentDeleteClient{Client: h.k8s, err: errors.New("deployment deletion blocked")}

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "removing connector")
	assert.Len(t, h.cf.Tunnels, 1, "tunnel deletion must be skipped while connector removal fails")
	assert.Len(t, h.cf.AccessApps, 1, "Access must remain while its tunnel still exists")
	assert.Contains(t, h.get().Finalizers, FinalizerName)

	h.r.Client = h.k8s
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.AccessApps)
}

// TestZoneMismatchFailsBeforeProvisioning ensures CR authors cannot choose
// an arbitrary Cloudflare zone through spec.cloudflare.zoneID.
func TestZoneMismatchFailsBeforeProvisioning(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.Cloudflare.ZoneID = "another-zone" })
	_, err := h.reconcile()
	require.NoError(t, err, "a policy violation is recorded rather than retried")
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
	assert.Contains(t, h.get().Status.Message, "does not match the operator's configured Cloudflare zone")
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestPublicTunnelRequiresNamespaceAllowlist ensures allowPublic is not an
// authorization decision available to every namespace by default.
func TestPublicTunnelRequiresNamespaceAllowlist(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Spec.Security = agentopsv1alpha1.SecurityConfig{AllowPublic: true}
	})
	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
	assert.Contains(t, h.get().Status.Message, "not permitted in namespace")
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestPublicTunnelAllowedOnlyInConfiguredNamespace covers the positive case
// and proves that the allowlist is namespace-specific.
func TestPublicTunnelAllowedOnlyInConfiguredNamespace(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Spec.Security = agentopsv1alpha1.SecurityConfig{AllowPublic: true}
	})
	h.r.PublicNamespaces = map[string]struct{}{"default": {}}
	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Equal(t, 1, h.cf.CreateTunnelCalls)
}

// TestRevokingPublicNamespaceCleansUpExistingTunnel verifies that changing
// operator policy also removes already-provisioned public endpoints.
func TestRevokingPublicNamespaceCleansUpExistingTunnel(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Spec.Security = agentopsv1alpha1.SecurityConfig{AllowPublic: true}
	})
	h.r.PublicNamespaces = map[string]struct{}{"default": {}}
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	require.Len(t, h.cf.Tunnels, 1)

	h.r.PublicNamespaces = nil
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseTerminated, h.get().Status.Phase)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.DNSRecords)
	assert.Empty(t, h.cf.AccessApps)
}

// TestPublicNamespaceAllowlistUsesExactNames prevents prefix or suffix
// matches from granting unauthenticated access to an unintended namespace.
func TestPublicNamespaceAllowlistUsesExactNames(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Namespace = "agent-public-extra"
		cr.Spec.Security = agentopsv1alpha1.SecurityConfig{AllowPublic: true}
	})
	h.r.PublicNamespaces = map[string]struct{}{"agent-public": {}}
	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
	assert.Zero(t, h.cf.CreateTunnelCalls)
}

// TestAccessPolicyDriftTerminatesTunnelFailsClosed verifies that externally
// deleted or modified Access policies cannot leave an Active hostname routed.
func TestAccessPolicyDriftTerminatesTunnelFailsClosed(t *testing.T) {
	cases := map[string]func(*harness){
		"application deleted": func(h *harness) { delete(h.cf.AccessApps, h.get().Status.AccessAppID) },
		"public destination override": func(h *harness) {
			app := h.cf.AccessApps[h.get().Status.AccessAppID]
			app.HasPublicOverride = true
			h.cf.AccessApps[h.get().Status.AccessAppID] = app
		},
		"allow policy removed": func(h *harness) {
			app := h.cf.AccessApps[h.get().Status.AccessAppID]
			app.Emails = nil
			h.cf.AccessApps[h.get().Status.AccessAppID] = app
		},
		"allowlist broadened": func(h *harness) {
			app := h.cf.AccessApps[h.get().Status.AccessAppID]
			app.Emails = append(app.Emails, "attacker@example.com")
			h.cf.AccessApps[h.get().Status.AccessAppID] = app
		},
		"allowed identity changed": func(h *harness) {
			app := h.cf.AccessApps[h.get().Status.AccessAppID]
			app.Emails = []string{"other@example.com"}
			h.cf.AccessApps[h.get().Status.AccessAppID] = app
		},
		"public path override added": func(h *harness) {
			app := h.cf.AccessApps[h.get().Status.AccessAppID]
			app.HasPublicOverride = true
			h.cf.AccessApps[h.get().Status.AccessAppID] = app
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			_, err := h.reconcile()
			require.NoError(t, err)
			require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
			require.NotEmpty(t, h.cf.DNSRecords)

			mutate(h)
			// Access policy verification is intentionally scheduled; advance the
			// fake clock so this test exercises drift detection immediately.
			current := h.get()
			require.NotNil(t, current.Status.AccessPolicyNextCheckAt)
			h.now = current.Status.AccessPolicyNextCheckAt.Time
			_, err = h.reconcile()
			require.NoError(t, err, "successful fail-closed cleanup should converge")
			got := h.get()
			assert.Equal(t, agentopsv1alpha1.PhaseTerminated, got.Status.Phase)
			assert.Contains(t, got.Status.Message, "policy drift detected")
			assert.Empty(t, h.cf.DNSRecords, "DNS routing must be removed")
			assert.Empty(t, h.cf.Tunnels, "the tunnel must be removed")
			assert.Empty(t, h.cf.AccessApps, "the recorded Access application must be cleaned up")
		})
	}
}

// TestActiveTunnelKeepsResourcesWhenAccessVerificationFails verifies that a
// transient Cloudflare API error is not mistaken for confirmed policy drift.
func TestActiveTunnelKeepsResourcesWhenAccessVerificationFails(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)

	active := h.get()
	require.NotEmpty(t, active.Status.TunnelID)
	require.NotEmpty(t, active.Status.AccessAppID)
	require.Equal(t, active.Status.TunnelID, h.cf.DNSRecords[active.Status.Hostname])
	require.Len(t, h.cf.Tunnels, 1)
	require.Len(t, h.cf.AccessApps, 1)

	require.NotNil(t, active.Status.AccessPolicyNextCheckAt)
	h.now = active.Status.AccessPolicyNextCheckAt.Time
	h.cf.FindAccessErr = errors.New("Cloudflare API temporarily unavailable")
	_, err = h.reconcile()
	require.Error(t, err)

	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.True(t, strings.HasPrefix(got.Status.Message, "UNPROTECTED:"), "message should explain that policy verification failed")
	assert.Equal(t, map[string]string{active.Status.Hostname: active.Status.TunnelID}, h.cf.DNSRecords)
	assert.Contains(t, h.cf.Tunnels, active.Status.TunnelID)
	assert.Len(t, h.cf.Tunnels, 1)
	assert.Contains(t, h.cf.AccessApps, active.Status.AccessAppID)
	assert.Len(t, h.cf.AccessApps, 1)
}

// TestAccessPolicyVerificationIsJitteredAndRateLimited verifies that frequent
// session reconciles reuse the last verified policy, and Cloudflare 429s defer
// the next verification without terminating or marking the tunnel unprotected.
func TestAccessPolicyVerificationIsJitteredAndRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	active := h.get()
	require.Equal(t, agentopsv1alpha1.PhaseActive, active.Status.Phase)
	require.NotNil(t, active.Status.AccessPolicyVerifiedAt)
	require.NotNil(t, active.Status.AccessPolicyNextCheckAt)
	assert.Zero(t, h.cf.VerifyAccessCalls, "successful policy creation is the initial trusted verification")

	firstCheck := active.Status.AccessPolicyNextCheckAt.Time
	h.now = firstCheck.Add(-time.Second)
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Zero(t, h.cf.VerifyAccessCalls, "reconciles before the deadline must not call Cloudflare")

	// Simulate connector loss immediately before a due verification. A 429
	// must not restore it from retained credentials.
	deployment, err := h.deployment()
	require.NoError(t, err)
	require.NoError(t, h.k8s.Delete(context.Background(), deployment))

	h.now = firstCheck
	h.cf.FindAccessErr = &cf.HTTPStatusError{StatusCode: 429, Message: "Cloudflare API rate limit"}
	_, err = h.reconcile()
	require.NoError(t, err, "rate limiting is deferred, not treated as policy drift")
	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "rate-limited")
	assert.Equal(t, active.Status.AccessPolicyVerifiedAt, got.Status.AccessPolicyVerifiedAt, "429 must preserve the last successful verification")
	require.NotNil(t, got.Status.AccessPolicyNextCheckAt)
	assert.Equal(t, h.now.Add(accessPolicyRateLimitDelay(&active)).Truncate(time.Second), got.Status.AccessPolicyNextCheckAt.Time)
	assert.Equal(t, active.Status.TunnelID, h.cf.DNSRecords[active.Status.Hostname], "the existing route remains intact")
	_, deploymentErr := h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "a missing connector must not be restored while Access verification is rate-limited")
	assert.Equal(t, 1, h.cf.VerifyAccessCalls)

	h.cf.FindAccessErr = nil
	retryAt := got.Status.AccessPolicyNextCheckAt.Time
	h.now = retryAt.Add(-time.Second)
	_, err = h.reconcile()
	require.NoError(t, err)
	got = h.get()
	assert.Contains(t, got.Status.Message, "rate-limited", "the warning remains until a successful verification")
	assert.Equal(t, 1, h.cf.VerifyAccessCalls, "reconciles during backoff must not call Cloudflare")
	_, deploymentErr = h.deployment()
	assert.True(t, apierrors.IsNotFound(deploymentErr), "backoff reconciles must not restore the connector")

	h.now = retryAt
	_, err = h.reconcile()
	require.NoError(t, err)
	got = h.get()
	assert.Equal(t, 2, h.cf.VerifyAccessCalls, "verification resumes after the backoff")
	assert.Equal(t, h.now, got.Status.AccessPolicyVerifiedAt.Time)
	assert.True(t, got.Status.AccessPolicyNextCheckAt.Time.After(h.now))
	assert.NotContains(t, got.Status.Message, "rate-limited")
}

// TestInitialAccessRateLimitDoesNotRestoreConnector ensures an Active legacy
// tunnel without a recorded Access application cannot recreate its connector
// while initial Access setup is rate-limited.
func TestInitialAccessRateLimitDoesNotRestoreConnector(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	active := h.get()
	require.Equal(t, agentopsv1alpha1.PhaseActive, active.Status.Phase)
	require.NotEmpty(t, active.Status.AccessAppID)

	// Model an older/incomplete Active object and a missing connector.
	active.Status.AccessAppID = ""
	active.Status.AccessPolicyNextCheckAt = nil
	require.NoError(t, h.k8s.Status().Update(context.Background(), &active))
	deployment, err := h.deployment()
	require.NoError(t, err)
	require.NoError(t, h.k8s.Delete(context.Background(), deployment))
	secret, err := h.secret()
	require.NoError(t, err)
	require.NoError(t, h.k8s.Delete(context.Background(), secret))

	h.cf.FindAccessErr = &cf.HTTPStatusError{StatusCode: 429, Message: "Cloudflare API rate limit"}
	res, err := h.reconcile()
	require.NoError(t, err)
	require.Greater(t, res.RequeueAfter, time.Duration(0))
	got := h.get()
	assert.Equal(t, agentopsv1alpha1.PhaseActive, got.Status.Phase)
	assert.Empty(t, got.Status.AccessAppID)
	assert.Contains(t, got.Status.Message, "rate-limited")
	h.assertConnectorGone()

	// Reconciliation during backoff must not bypass the deadline or restore
	// the connector before the Access application has been established.
	_, err = h.reconcile()
	require.NoError(t, err)
	h.assertConnectorGone()
}

func TestActiveRequeueAfterPreservesLifecycleLimitsDuringAccessBackoff(t *testing.T) {
	now := t0
	deadline := metav1.NewTime(now.Add(90 * time.Second))
	expires := metav1.NewTime(now.Add(5 * time.Second))
	tunnel := &agentopsv1alpha1.AgentEdgeTunnel{
		Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{},
		Status: agentopsv1alpha1.AgentEdgeTunnelStatus{
			AccessPolicyNextCheckAt: &deadline,
			ExpiresAt:               &expires,
		},
	}
	assert.Equal(t, 5*time.Second, activeRequeueAfter(tunnel, now), "TTL cleanup must run before Access retry")

	tunnel.Status.ExpiresAt = nil
	tunnel.Spec.SessionRef = &agentopsv1alpha1.SessionReference{Name: "session"}
	assert.Equal(t, sessionPollInterval, activeRequeueAfter(tunnel, now), "session state must be polled before Access retry")

	tunnel.Spec.SessionRef = nil
	assert.Equal(t, maxRequeue, activeRequeueAfter(tunnel, now), "ordinary Active checks remain bounded by maxRequeue")
}

// TestAccessPolicyVerificationIntervalIsJittered ensures each tunnel receives
// a stable interval in the five-to-ten-minute range.
func TestAccessPolicyVerificationIntervalIsJittered(t *testing.T) {
	a := &agentopsv1alpha1.AgentEdgeTunnel{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "uid-a"}}
	b := &agentopsv1alpha1.AgentEdgeTunnel{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns", UID: "uid-b"}}
	for _, tunnel := range []*agentopsv1alpha1.AgentEdgeTunnel{a, b} {
		interval := accessPolicyVerificationInterval(tunnel)
		assert.GreaterOrEqual(t, interval, 5*time.Minute)
		assert.Less(t, interval, 10*time.Minute)
		assert.Equal(t, interval, accessPolicyVerificationInterval(tunnel), "jitter must be stable for a given tunnel")
	}
	assert.NotEqual(t, accessPolicyVerificationInterval(a), accessPolicyVerificationInterval(b))
}
