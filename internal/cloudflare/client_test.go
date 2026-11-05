package cloudflare

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveCloudflareTunnel(t *testing.T) {
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	zoneID := os.Getenv("CLOUDFLARE_ZONE_ID")
	// Full hostname inside the zone, e.g. ci-test.agents.example.com. The DNS
	// part of the test is skipped unless both zone and hostname are set.
	hostname := os.Getenv("CLOUDFLARE_TEST_HOSTNAME")

	if apiToken == "" || accountID == "" {
		t.Skip("Skipping live Cloudflare API test: CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID not set")
	}

	client, err := NewRealCloudflareClient(accountID, apiToken)
	require.NoError(t, err)

	ctx := context.Background()

	info, err := client.CreateTunnel(ctx, "ci-test-agent-tunnel")
	require.NoError(t, err)
	require.NotNil(t, info)
	// Always remove the tunnel, even if a later assertion fails.
	t.Cleanup(func() {
		assert.NoError(t, client.DeleteTunnel(context.Background(), info.ID))
	})

	if zoneID != "" && hostname != "" {
		require.NoError(t, client.CreateDNSRecord(ctx, zoneID, hostname, info.ID))
		// Creating again must be a no-op, not an error.
		require.NoError(t, client.CreateDNSRecord(ctx, zoneID, hostname, info.ID))
		assert.NoError(t, client.DeleteDNSRecord(ctx, zoneID, hostname, info.ID))
	}
}
