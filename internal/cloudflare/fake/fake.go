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
	FindTunnelsErr  error
	FindAccessErr   error
}

// AccessApp is a recorded Access application.
type AccessApp struct {
	Name   string
	Domain string
	Emails []string
}

// New returns an empty Client.
func New() *Client {
	return &Client{
		Tunnels:    make(map[string]*cloudflare.TunnelInfo),
		DNSRecords: make(map[string]string),
		AccessApps: make(map[string]AccessApp),
	}
}

// CreateTunnel records a tunnel under an ID that starts with
// cloudflare.DryRunIDPrefix, so it can never be mistaken for a real one.
func (c *Client) CreateTunnel(_ context.Context, name string) (*cloudflare.TunnelInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CreateTunnelCalls++
	if c.CreateTunnelErr != nil {
		return nil, c.CreateTunnelErr
	}
	id := fmt.Sprintf("%stunnel-%d", cloudflare.DryRunIDPrefix, c.CreateTunnelCalls)
	info := &cloudflare.TunnelInfo{
		AccountID: "fake-account",
		ID:        id,
		Name:      name,
		Secret:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Hostname:  fmt.Sprintf("%s.cfargotunnel.com", id),
	}
	c.Tunnels[id] = info
	return info, nil
}

// CreateDNSRecord records hostname as pointing at tunnelID. Like the real
// client, it is idempotent for the same tunnel and refuses to take over a
// hostname that already points somewhere else.
func (c *Client) CreateDNSRecord(_ context.Context, _, hostname, tunnelID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.CreateDNSErr != nil {
		return c.CreateDNSErr
	}
	if owner, ok := c.DNSRecords[hostname]; ok && owner != tunnelID {
		return fmt.Errorf("DNS record %q already exists and points at %q, not %q", hostname, owner, tunnelID)
	}
	c.DNSRecords[hostname] = tunnelID
	return nil
}

// CreateAccessPolicy records the allowed emails for domain and returns the ID of
// the new application. The ID starts with cloudflare.DryRunIDPrefix.
func (c *Client) CreateAccessPolicy(_ context.Context, _, name, domain string, allowedEmails []string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CreateAccessCalls++
	if c.CreateAccessErr != nil {
		return "", c.CreateAccessErr
	}
	id := fmt.Sprintf("%saccess-%d", cloudflare.DryRunIDPrefix, c.CreateAccessCalls)
	c.AccessApps[id] = AccessApp{Name: name, Domain: domain, Emails: allowedEmails}
	return id, nil
}

// DeleteTunnel forgets the tunnel. Deleting an unknown ID succeeds, like the
// real client, which treats an already-absent tunnel as deleted.
func (c *Client) DeleteTunnel(_ context.Context, tunnelID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DeleteTunnelErr != nil {
		return c.DeleteTunnelErr
	}
	delete(c.Tunnels, tunnelID)
	return nil
}

// DeleteDNSRecord forgets hostname, but only if it points at tunnelID.
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

// DeleteAccessApplication forgets the application with this ID. Deleting an
// unknown ID succeeds, like the real client.
func (c *Client) DeleteAccessApplication(_ context.Context, _, appID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DeleteAccessErr != nil {
		return c.DeleteAccessErr
	}
	delete(c.AccessApps, appID)
	return nil
}

// FindTunnels returns the recorded tunnels with exactly this name.
func (c *Client) FindTunnels(_ context.Context, name string) ([]cloudflare.TunnelRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.FindTunnelsErr != nil {
		return nil, c.FindTunnelsErr
	}
	var out []cloudflare.TunnelRef
	for id, t := range c.Tunnels {
		if t.Name == name {
			out = append(out, cloudflare.TunnelRef{ID: id, Name: t.Name})
		}
	}
	return out, nil
}

// VerifyAccessPolicy checks that the recorded application has exactly the requested email allowlist.
func (c *Client) VerifyAccessPolicy(_ context.Context, _, appID, name, domain string, allowedEmails []string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.FindAccessErr != nil {
		return false, c.FindAccessErr
	}
	app, ok := c.AccessApps[appID]
	if !ok || app.Name != name || app.Domain != domain || len(app.Emails) != len(allowedEmails) {
		return false, nil
	}
	want := make(map[string]struct{}, len(allowedEmails))
	for _, email := range allowedEmails {
		want[email] = struct{}{}
	}
	if len(want) != len(allowedEmails) {
		return false, nil
	}
	for _, email := range app.Emails {
		if _, ok := want[email]; !ok {
			return false, nil
		}
		delete(want, email)
	}
	return len(want) == 0, nil
}

// FindAccessApplications returns the recorded applications whose name and
// domain match. An application with no emails has no allow policy.
func (c *Client) FindAccessApplications(_ context.Context, _, name, domain string) ([]cloudflare.AccessAppRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.FindAccessErr != nil {
		return nil, c.FindAccessErr
	}
	var out []cloudflare.AccessAppRef
	for id, a := range c.AccessApps {
		if a.Name == name && a.Domain == domain {
			out = append(out, cloudflare.AccessAppRef{ID: id, HasAllowPolicy: len(a.Emails) > 0})
		}
	}
	return out, nil
}
