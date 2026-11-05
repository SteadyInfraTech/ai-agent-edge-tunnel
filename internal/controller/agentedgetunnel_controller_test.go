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
	k8s := fakeclient.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(append([]client.Object{cr}, objs...)...).
		WithStatusSubresource(cr, &appsv1.Deployment{}).
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
		Scheme:           scheme.Scheme,
		CloudflareClient: h.cf,
		AccountID:        "fake-account",
		BaseDomain:       "agents.example.com",
		ConnectorImage:   testImage,
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

func TestPublicTunnelSkipsAccess(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) {
		cr.Spec.Security = agentopsv1alpha1.SecurityConfig{AllowPublic: true}
	})

	_, err := h.reconcile()
	require.NoError(t, err)

	assert.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)
	assert.Zero(t, h.cf.CreateAccessCalls)
	assert.Empty(t, h.get().Status.AccessAppID)
	assert.NotEmpty(t, h.cf.DNSRecords)
}

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
}

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
	// The connector goes first: Cloudflare refuses to delete a tunnel that is
	// still connected, so stopping it is what lets the retry succeed.
	h.assertConnectorGone()

	h.cf.DeleteTunnelErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.Empty(t, h.cf.Tunnels)
	assert.Empty(t, h.cf.AccessApps)
}

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

func TestEmptyTTLMeansNoExpiry(t *testing.T) {
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Spec.TTL = "" })
	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Nil(t, h.get().Status.ExpiresAt)
}

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
