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

// TestCheckExistingRecord checks: check existing record.
func TestCheckExistingRecord(t *testing.T) {
	yes, no := true, false
	rec := func(typ, content string, proxied *bool) cloudflare.DNSRecord {
		return cloudflare.DNSRecord{Type: typ, Content: content, Proxied: proxied}
	}
	target := tunnelTarget("t1")

	assert.NoError(t, checkExistingRecord(rec("CNAME", target, &yes), "h.example.com", "t1"))
	assert.Error(t, checkExistingRecord(rec("CNAME", target, &no), "h.example.com", "t1"), "DNS-only record carries no traffic")
	assert.Error(t, checkExistingRecord(rec("CNAME", target, nil), "h.example.com", "t1"), "unknown proxy state is not ready")
	assert.Error(t, checkExistingRecord(rec("CNAME", tunnelTarget("other"), &yes), "h.example.com", "t1"), "other tunnel")
	assert.Error(t, checkExistingRecord(rec("A", target, &yes), "h.example.com", "t1"), "wrong type")
}

// TestSelectCNAMEIgnoresOtherRecordTypesAndOrder checks: select c n a m e ignores other record types and order.
func TestSelectCNAMEIgnoresOtherRecordTypesAndOrder(t *testing.T) {
	yes := true
	txt := cloudflare.DNSRecord{Type: "TXT", Content: "v=spf1 -all"}
	good := cloudflare.DNSRecord{Type: "CNAME", Content: tunnelTarget("t1"), Proxied: &yes}
	foreign := cloudflare.DNSRecord{Type: "CNAME", Content: tunnelTarget("other"), Proxied: &yes}

	// The TXT record listed first must not hide the valid CNAME.
	rec, ok := selectCNAME([]cloudflare.DNSRecord{txt, good}, "t1")
	require.True(t, ok)
	assert.Equal(t, good.Content, rec.Content)
	assert.NoError(t, checkExistingRecord(rec, "h.example.com", "t1"))

	// A CNAME for this tunnel wins over one for another tunnel, in either order.
	for _, recs := range [][]cloudflare.DNSRecord{{foreign, good}, {good, foreign}} {
		rec, ok := selectCNAME(recs, "t1")
		require.True(t, ok)
		assert.Equal(t, good.Content, rec.Content)
	}

	// Only a foreign CNAME: reported, so it is rejected rather than overwritten.
	rec, ok = selectCNAME([]cloudflare.DNSRecord{txt, foreign}, "t1")
	require.True(t, ok)
	assert.Error(t, checkExistingRecord(rec, "h.example.com", "t1"))

	// Only a TXT record: no CNAME yet, so one can be created beside it.
	_, ok = selectCNAME([]cloudflare.DNSRecord{txt}, "t1")
	assert.False(t, ok)
	_, ok = selectCNAME(nil, "t1")
	assert.False(t, ok)
}

// TestIsNotFound checks: is not found.
func TestIsNotFound(t *testing.T) {
	notFound := cloudflare.NewNotFoundError(&cloudflare.Error{StatusCode: 404, Type: cloudflare.ErrorTypeNotFound})
	serverErr := cloudflare.NewServiceError(&cloudflare.Error{StatusCode: 500, Type: cloudflare.ErrorTypeService})

	assert.True(t, isNotFound(notFound))
	assert.True(t, isNotFound(fmt.Errorf("wrapped: %w", notFound)), "must see through wrapping")
	assert.False(t, isNotFound(serverErr), "5xx must still be retried")
	assert.False(t, isNotFound(errors.New("boom")))
	assert.False(t, isNotFound(nil))
}

// TestLiveCloudflareTunnel checks: live cloudflare tunnel.
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

	tunnelName := "ci-test-agent-tunnel-" + suffix
	info, err := client.CreateTunnel(ctx, tunnelName)
	require.NoError(t, err)
	require.NotNil(t, info)
	// Always remove the tunnel, even if a later assertion fails.
	t.Cleanup(func() {
		assert.NoError(t, client.DeleteTunnel(context.Background(), info.ID))
		// A deleted tunnel must no longer be found, or recovery would adopt it.
		gone, err := client.FindTunnels(context.Background(), tunnelName)
		assert.NoError(t, err)
		assert.Empty(t, gone, "a deleted tunnel must not be listed")
	})

	// The recovery path finds a tunnel by its exact name.
	found, err := client.FindTunnels(ctx, tunnelName)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, info.ID, found[0].ID)
	none, err := client.FindTunnels(ctx, tunnelName+"-nope")
	require.NoError(t, err)
	assert.Empty(t, none, "only an exact name may match")

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
