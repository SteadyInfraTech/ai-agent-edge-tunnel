package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
)

func TestReconcileAgentEdgeTunnel(t *testing.T) {
	_ = agentopsv1alpha1.AddToScheme(scheme.Scheme)

	cr := &agentopsv1alpha1.AgentEdgeTunnel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent-tunnel",
			Namespace: "default",
		},
		Spec: agentopsv1alpha1.AgentEdgeTunnelSpec{
			TargetPodSelector: map[string]string{"app": "langgraph-agent"},
			Cloudflare: agentopsv1alpha1.CloudflareConfig{
				ZoneID:    "mock-zone-id",
				Subdomain: "agent-job-001",
			},
			TTL:             "1h",
			CleanupOnFinish: true,
		},
	}

	fakeClient := fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	mockCF := cf.NewMockCloudflareClient()

	reconciler := &AgentEdgeTunnelReconciler{
		Client:           fakeClient,
		Scheme:           scheme.Scheme,
		CloudflareClient: mockCF,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-agent-tunnel",
			Namespace: "default",
		},
	}

	ctx := context.Background()

	// First Reconcile Pass
	res, err := reconciler.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.False(t, res.IsZero())

	// Verify Status
	var updatedCR agentopsv1alpha1.AgentEdgeTunnel
	err = fakeClient.Get(ctx, req.NamespacedName, &updatedCR)
	assert.NoError(t, err)
	assert.Equal(t, "Active", updatedCR.Status.Phase)
	assert.Equal(t, "agent-job-001.agents.yourdomain.com", updatedCR.Status.Hostname)
	assert.NotEmpty(t, updatedCR.Status.TunnelID)
}
