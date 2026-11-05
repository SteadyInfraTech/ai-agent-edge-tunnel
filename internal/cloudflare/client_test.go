package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsNotFound(t *testing.T) {
	notFound := cloudflare.NewNotFoundError(&cloudflare.Error{StatusCode: 404, Type: cloudflare.ErrorTypeNotFound})
	serverErr := cloudflare.NewServiceError(&cloudflare.Error{StatusCode: 500, Type: cloudflare.ErrorTypeService})

	assert.True(t, isNotFound(notFound))
	assert.True(t, isNotFound(fmt.Errorf("wrapped: %w", notFound)), "must see through wrapping")
	assert.False(t, isNotFound(serverErr), "5xx must still be retried")
	assert.False(t, isNotFound(errors.New("boom")))
	assert.False(t, isNotFound(nil))
}

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

	// Unique per run: Cloudflare rejects duplicate tunnel names and DNS records,
	// so overlapping runs (or a leftover from a crashed one) must not collide.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	if hostname != "" {
		hostname = suffix + "-" + hostname
	}

	info, err := client.CreateTunnel(ctx, "ci-test-agent-tunnel-"+suffix)
	require.NoError(t, err)
	require.NotNil(t, info)
	// Always remove the tunnel, even if a later assertion fails.
	t.Cleanup(func() {
		assert.NoError(t, client.DeleteTunnel(context.Background(), info.ID))
	})

	if zoneID != "" && hostname != "" {
		// Registered before creation so a failed second create or a failed
		// explicit delete below still removes the record. DeleteDNSRecord is a
		// no-op once the record is gone.
		t.Cleanup(func() {
			assert.NoError(t, client.DeleteDNSRecord(context.Background(), zoneID, hostname, info.ID))
		})
		require.NoError(t, client.CreateDNSRecord(ctx, zoneID, hostname, info.ID))
		// Creating again must be a no-op, not an error.
		require.NoError(t, client.CreateDNSRecord(ctx, zoneID, hostname, info.ID))
		assert.NoError(t, client.DeleteDNSRecord(ctx, zoneID, hostname, info.ID))
	}
}
