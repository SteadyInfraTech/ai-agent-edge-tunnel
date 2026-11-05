// Package fake provides an in-memory cloudflare.TunnelService. It is used by
// tests and by the operator's --dry-run mode; it never talks to Cloudflare.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
)

var _ cloudflare.TunnelService = (*Client)(nil)

// Client records what would have been created. The *Err fields inject
// failures, and CreateTunnelCalls lets tests assert on idempotency.
type Client struct {
	mu sync.Mutex

	Tunnels    map[string]*cloudflare.TunnelInfo // by tunnel ID
	DNSRecords map[string]string                 // hostname -> tunnel ID
	AccessApps map[string]AccessApp              // app ID -> app

	CreateTunnelCalls int
	CreateAccessCalls int

	CreateTunnelErr error
	CreateDNSErr    error
	CreateAccessErr error
	DeleteTunnelErr error
	DeleteDNSErr    error
	DeleteAccessErr error
}

// AccessApp is a recorded Access application.
type AccessApp struct {
	Domain string
	Emails []string
}

func New() *Client {
	return &Client{
		Tunnels:    make(map[string]*cloudflare.TunnelInfo),
		DNSRecords: make(map[string]string),
		AccessApps: make(map[string]AccessApp),
	}
}

func (c *Client) CreateTunnel(_ context.Context, name string) (*cloudflare.TunnelInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CreateTunnelCalls++
	if c.CreateTunnelErr != nil {
		return nil, c.CreateTunnelErr
	}
	id := fmt.Sprintf("fake-tunnel-%d", c.CreateTunnelCalls)
	info := &cloudflare.TunnelInfo{
		AccountID: "fake-account",
		ID:        id,
		Name:      name,
		Secret:    "fake-secret",
		Hostname:  fmt.Sprintf("%s.cfargotunnel.com", id),
		Token:     "fake-token",
	}
	c.Tunnels[id] = info
	return info, nil
}

func (c *Client) CreateDNSRecord(_ context.Context, _, hostname, tunnelID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.CreateDNSErr != nil {
		return c.CreateDNSErr
	}
	c.DNSRecords[hostname] = tunnelID
	return nil
}

func (c *Client) CreateAccessPolicy(_ context.Context, _, _, domain string, allowedEmails []string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CreateAccessCalls++
	if c.CreateAccessErr != nil {
		return "", c.CreateAccessErr
	}
	id := fmt.Sprintf("fake-access-%d", c.CreateAccessCalls)
	c.AccessApps[id] = AccessApp{Domain: domain, Emails: allowedEmails}
	return id, nil
}

func (c *Client) DeleteTunnel(_ context.Context, tunnelID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DeleteTunnelErr != nil {
		return c.DeleteTunnelErr
	}
	delete(c.Tunnels, tunnelID)
	return nil
}

func (c *Client) DeleteDNSRecord(_ context.Context, _, hostname, tunnelID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DeleteDNSErr != nil {
		return c.DeleteDNSErr
	}
	if c.DNSRecords[hostname] == tunnelID {
		delete(c.DNSRecords, hostname)
	}
	return nil
}

func (c *Client) DeleteAccessApplication(_ context.Context, _, appID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DeleteAccessErr != nil {
		return c.DeleteAccessErr
	}
	delete(c.AccessApps, appID)
	return nil
}
