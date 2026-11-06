package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
)

// TestLongNameWithServiceFailsBeforeCreatingAnything checks: a name too long
// for a label value is rejected up front when a connector would be created.
func TestLongNameWithServiceFailsBeforeCreatingAnything(t *testing.T) {
	long := strings.Repeat("a", 64)
	h := newHarness(t, func(cr *agentopsv1alpha1.AgentEdgeTunnel) { cr.Name = long })
	h.req.Name = long

	_, err := h.reconcile()
	require.NoError(t, err, "a spec error is not retried")
	assert.Equal(t, agentopsv1alpha1.PhaseFailed, h.get().Status.Phase)
	assert.Contains(t, h.get().Status.Message, "metadata.name")
	assert.Zero(t, h.cf.CreateTunnelCalls)
	assert.Zero(t, h.cf.CreateAccessCalls)
	assert.Empty(t, h.cf.DNSRecords)
}
