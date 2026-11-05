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
