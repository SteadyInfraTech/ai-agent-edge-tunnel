package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFakeOnlyDeletesRecordsOwnedByTunnel checks: fake only deletes records owned by tunnel.
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

// TestFakeRejectsHostnameOwnedByAnotherTunnel checks: fake rejects hostname owned by another tunnel.
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
