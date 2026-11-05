package cloudflare

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockCloudflareClient(t *testing.T) {
	mock := NewMockCloudflareClient()
	ctx := context.Background()

	info, err := mock.CreateTunnel(ctx, "agent-job-123")
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "agent-job-123", info.Name)
	assert.Contains(t, info.ID, "mock-id-agent-job-123")

	err = mock.CreateDNSRecord(ctx, "zone-123", "agent-job-123.example.com", info.ID)
	assert.NoError(t, err)
	assert.Equal(t, info.ID, mock.CreatedDNSRecords["agent-job-123.example.com"])

	err = mock.DeleteDNSRecord(ctx, "zone-123", "agent-job-123.example.com")
	assert.NoError(t, err)
	assert.Empty(t, mock.CreatedDNSRecords["agent-job-123.example.com"])
}

func TestLiveCloudflareTunnel(t *testing.T) {
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	zoneID := os.Getenv("CLOUDFLARE_ZONE_ID")

	if apiToken == "" || accountID == "" {
		t.Skip("Skipping live Cloudflare API test: CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID not set")
	}

	client, err := NewRealCloudflareClient(accountID, apiToken)
	require.NoError(t, err)

	ctx := context.Background()

	info, err := client.CreateTunnel(ctx, "ci-test-agent-tunnel")
	require.NoError(t, err)
	require.NotNil(t, info)

	if zoneID != "" {
		err = client.CreateDNSRecord(ctx, zoneID, "ci-test.agents.yourdomain.com", info.ID)
		assert.NoError(t, err)

		err = client.DeleteDNSRecord(ctx, zoneID, "ci-test.agents.yourdomain.com")
		assert.NoError(t, err)
	}

	err = client.DeleteTunnel(ctx, info.ID)
	assert.NoError(t, err)
}
