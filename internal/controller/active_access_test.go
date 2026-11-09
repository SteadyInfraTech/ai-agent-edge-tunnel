package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
)

// TestActiveTunnelWithoutAccessAppGetsProtected checks: an Active, email-protected
// object that has no Access application is protected on the next reconcile and
// is reported as unprotected until then.
func TestActiveTunnelWithoutAccessAppGetsProtected(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, agentopsv1alpha1.PhaseActive, h.get().Status.Phase)

	// Simulate an object that went Active before Access applications existed.
	cr := h.get()
	cr.Status.AccessAppID = ""
	require.NoError(t, h.k8s.Status().Update(context.Background(), &cr))
	clear(h.cf.AccessApps) // no application exists in Cloudflare either
	h.cf.CreateAccessErr = errors.New("access api down")

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, h.get().Status.Message, "UNPROTECTED")
	assert.Empty(t, h.get().Status.AccessAppID)

	h.cf.CreateAccessErr = nil
	_, err = h.reconcile()
	require.NoError(t, err)
	assert.NotEmpty(t, h.get().Status.AccessAppID)
	assert.NotContains(t, h.get().Status.Message, "UNPROTECTED")
}

// TestActiveTunnelThatLostItsAccessAppIDLeavesTheApplicationUntouched checks
// that a matching application is not adopted or deleted without independent
// ownership evidence, even when it is currently protecting an active hostname.
func TestActiveTunnelThatLostItsAccessAppIDLeavesTheApplicationUntouched(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.reconcile()
	require.NoError(t, err)
	appID := h.get().Status.AccessAppID
	require.NotEmpty(t, appID)

	cr := h.get()
	cr.Status.AccessAppID = ""
	require.NoError(t, h.k8s.Status().Update(context.Background(), &cr))

	_, err = h.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ownership and policy identity cannot be established")
	assert.Empty(t, h.get().Status.AccessAppID, "do not adopt an application without ownership proof")
	assert.Equal(t, 1, h.cf.CreateAccessCalls, "do not create a duplicate application")
	assert.Contains(t, h.cf.AccessApps, appID, "the protecting application must remain untouched")
}
