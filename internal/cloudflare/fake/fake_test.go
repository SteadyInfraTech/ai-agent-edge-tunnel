package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFakeOnlyDeletesRecordsOwnedByTunnel(t *testing.T) {
	c := New()
	ctx := context.Background()

	info, err := c.CreateTunnel(ctx, "agent-job-123")
	require.NoError(t, err)
	require.NoError(t, c.CreateDNSRecord(ctx, "zone", "a.example.com", info.ID))
	assert.Equal(t, info.ID, c.DNSRecords["a.example.com"])

	// A different tunnel must not be able to delete this record.
	require.NoError(t, c.DeleteDNSRecord(ctx, "zone", "a.example.com", "someone-else"))
	assert.Contains(t, c.DNSRecords, "a.example.com")

	require.NoError(t, c.DeleteDNSRecord(ctx, "zone", "a.example.com", info.ID))
	assert.NotContains(t, c.DNSRecords, "a.example.com")

	require.NoError(t, c.DeleteTunnel(ctx, info.ID))
	assert.Empty(t, c.Tunnels)
}

func TestFakeAccessApplicationLifecycle(t *testing.T) {
	c := New()
	ctx := context.Background()

	id, err := c.CreateAccessPolicy(ctx, "acct", "name", "a.example.com", []string{"dev@example.com"})
	require.NoError(t, err)
	assert.Equal(t, "a.example.com", c.AccessApps[id].Domain)

	// Deletion is by ID only, and deleting something already gone is fine.
	require.NoError(t, c.DeleteAccessApplication(ctx, "acct", "not-an-id"))
	assert.Contains(t, c.AccessApps, id)
	require.NoError(t, c.DeleteAccessApplication(ctx, "acct", id))
	require.NoError(t, c.DeleteAccessApplication(ctx, "acct", id))
	assert.Empty(t, c.AccessApps)
}

func TestFakeRejectsHostnameOwnedByAnotherTunnel(t *testing.T) {
	c := New()
	ctx := context.Background()

	require.NoError(t, c.CreateDNSRecord(ctx, "zone", "a.example.com", "tunnel-1"))
	// Same tunnel again: idempotent.
	require.NoError(t, c.CreateDNSRecord(ctx, "zone", "a.example.com", "tunnel-1"))

	err := c.CreateDNSRecord(ctx, "zone", "a.example.com", "tunnel-2")
	require.Error(t, err)
	assert.Equal(t, "tunnel-1", c.DNSRecords["a.example.com"], "the first owner must be kept")
}
