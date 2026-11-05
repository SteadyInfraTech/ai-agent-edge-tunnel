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
	Policies   map[string][]string               // domain -> allowed emails

	CreateTunnelCalls int

	CreateTunnelErr error
	CreateDNSErr    error
	DeleteTunnelErr error
	DeleteDNSErr    error
}

func New() *Client {
	return &Client{
		Tunnels:    make(map[string]*cloudflare.TunnelInfo),
		DNSRecords: make(map[string]string),
		Policies:   make(map[string][]string),
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
		ID:       id,
		Name:     name,
		Secret:   "fake-secret",
		Hostname: fmt.Sprintf("%s.cfargotunnel.com", id),
		Token:    "fake-token",
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

func (c *Client) CreateAccessPolicy(_ context.Context, _, _, domain string, allowedEmails []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Policies[domain] = allowedEmails
	return nil
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

func (c *Client) DeleteAccessPolicy(_ context.Context, _, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.Policies, domain)
	return nil
}
