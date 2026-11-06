package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
)

// specUpdateCounter counts typed Updates of the AgentEdgeTunnel itself.
type specUpdateCounter struct {
	client.Client
	updates int
}

// Update counts writes of the main resource and passes everything through.
func (c *specUpdateCounter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*agentopsv1alpha1.AgentEdgeTunnel); ok {
		c.updates++
	}
	return c.Client.Update(ctx, obj, opts...)
}

// TestFinalizerChangesNeverRewriteTheSpec checks: finalizers are added and
// removed with a patch, because a typed Update would drop explicit false or
// empty spec fields and the CRD immutability rules would then block deletion.
func TestFinalizerChangesNeverRewriteTheSpec(t *testing.T) {
	h := newHarness(t, nil)
	counter := &specUpdateCounter{Client: h.k8s}
	h.r.Client = counter

	_, err := h.reconcile()
	require.NoError(t, err)
	assert.Contains(t, h.get().Finalizers, FinalizerName)

	cr := h.get()
	require.NoError(t, h.k8s.Delete(context.Background(), &cr))
	_, err = h.reconcile()
	require.NoError(t, err)

	var gone agentopsv1alpha1.AgentEdgeTunnel
	err = h.k8s.Get(context.Background(), h.req.NamespacedName, &gone)
	assert.True(t, apierrors.IsNotFound(err), "the finalizer must be released")
	assert.Zero(t, counter.updates, "the object must never be written back with a typed Update")
}
