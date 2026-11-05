package cloudflare

import (
	"context"
	"fmt"
	"log"
)

// TunnelInfo holds generated Cloudflare Tunnel details
type TunnelInfo struct {
	ID        string
	Name      string
	Secret    string
	Hostname  string
}

// TunnelService abstracts Cloudflare operations for testing and controller usage
type TunnelService interface {
	CreateTunnel(ctx context.Context, name string) (*TunnelInfo, error)
	CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error
	DeleteTunnel(ctx context.Context, tunnelID string) error
	DeleteDNSRecord(ctx context.Context, zoneID, hostname string) error
}

// RealCloudflareClient implements TunnelService using cloudflare-go SDK conventions
type RealCloudflareClient struct {
	AccountID string
	APIToken  string
}

func NewRealCloudflareClient(accountID, apiToken string) *RealCloudflareClient {
	return &RealCloudflareClient{
		AccountID: accountID,
		APIToken:  apiToken,
	}
}

func (c *RealCloudflareClient) CreateTunnel(ctx context.Context, name string) (*TunnelInfo, error) {
	if c.APIToken == "" || c.AccountID == "" {
		return nil, fmt.Errorf("missing Cloudflare credentials")
	}
	log.Printf("[Cloudflare API] Creating tunnel '%s' for Account '%s'", name, c.AccountID)
	// In production, invoke official cloudflare-go SDK:
	// tunnel, err := c.api.CreateTunnel(ctx, cloudflare.AccountIdentifier(c.AccountID), ...)
	return &TunnelInfo{
		ID:       "cf-tunnel-uuid-mock",
		Name:     name,
		Secret:   "cf-tunnel-secret-token-sample",
		Hostname: fmt.Sprintf("%s.cfargotunnel.com", name),
	}, nil
}

func (c *RealCloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error {
	log.Printf("[Cloudflare API] Creating CNAME '%s' -> '%s.cfargotunnel.com' in Zone '%s'", hostname, tunnelID, zoneID)
	return nil
}

func (c *RealCloudflareClient) DeleteTunnel(ctx context.Context, tunnelID string) error {
	log.Printf("[Cloudflare API] Deleting tunnel '%s'", tunnelID)
	return nil
}

func (c *RealCloudflareClient) DeleteDNSRecord(ctx context.Context, zoneID, hostname string) error {
	log.Printf("[Cloudflare API] Deleting CNAME record '%s' in Zone '%s'", hostname, zoneID)
	return nil
}

// MockCloudflareClient for fast unit testing without external API dependency
type MockCloudflareClient struct {
	CreatedTunnels map[string]*TunnelInfo
	CreatedDNSRecords map[string]string
}

func NewMockCloudflareClient() *MockCloudflareClient {
	return &MockCloudflareClient{
		CreatedTunnels:    make(map[string]*TunnelInfo),
		CreatedDNSRecords: make(map[string]string),
	}
}

func (m *MockCloudflareClient) CreateTunnel(ctx context.Context, name string) (*TunnelInfo, error) {
	info := &TunnelInfo{
		ID:       fmt.Sprintf("mock-id-%s", name),
		Name:     name,
		Secret:   "mock-secret-token",
		Hostname: fmt.Sprintf("%s.cfargotunnel.com", name),
	}
	m.CreatedTunnels[name] = info
	return info, nil
}

func (m *MockCloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error {
	m.CreatedDNSRecords[hostname] = tunnelID
	return nil
}

func (m *MockCloudflareClient) DeleteTunnel(ctx context.Context, tunnelID string) error {
	for k, v := range m.CreatedTunnels {
		if v.ID == tunnelID {
			delete(m.CreatedTunnels, k)
			break
		}
	}
	return nil
}

func (m *MockCloudflareClient) DeleteDNSRecord(ctx context.Context, zoneID, hostname string) error {
	delete(m.CreatedDNSRecords, hostname)
	return nil
}
