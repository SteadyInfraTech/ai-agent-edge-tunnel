package cloudflare

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/cloudflare/cloudflare-go"
)

type TunnelInfo struct {
	ID       string
	Name     string
	Secret   string
	Hostname string
	Token    string
}

type TunnelService interface {
	CreateTunnel(ctx context.Context, name string) (*TunnelInfo, error)
	CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error
	CreateAccessPolicy(ctx context.Context, accountID, name, domain string, allowedEmails []string) error
	DeleteTunnel(ctx context.Context, tunnelID string) error
	// DeleteDNSRecord removes only CNAME records for hostname that point at
	// tunnelID, so it can never delete a record this operator did not create.
	DeleteDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error
	DeleteAccessPolicy(ctx context.Context, accountID, domain string) error
}

type RealCloudflareClient struct {
	AccountID string
	APIToken  string
	api       *cloudflare.API
}

func NewRealCloudflareClient(accountID, apiToken string) (*RealCloudflareClient, error) {
	api, err := cloudflare.NewWithAPIToken(apiToken)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Cloudflare client: %w", err)
	}
	return &RealCloudflareClient{
		AccountID: accountID,
		APIToken:  apiToken,
		api:       api,
	}, nil
}

func (c *RealCloudflareClient) CreateTunnel(ctx context.Context, name string) (*TunnelInfo, error) {
	// Generate a cryptographically secure 32-byte secret and base64-encode it
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("failed to generate random secret: %w", err)
	}
	secret := base64.StdEncoding.EncodeToString(secretBytes)

	tunnel, err := c.api.CreateTunnel(ctx, cloudflare.AccountIdentifier(c.AccountID), cloudflare.TunnelCreateParams{
		Name:   name,
		Secret: secret,
	})
	if err != nil {
		return nil, fmt.Errorf("cloudflare API tunnel creation failed: %w", err)
	}

	token, err := c.api.GetTunnelToken(ctx, cloudflare.AccountIdentifier(c.AccountID), tunnel.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tunnel token: %w", err)
	}

	return &TunnelInfo{
		ID:       tunnel.ID,
		Name:     tunnel.Name,
		Secret:   secret,
		Hostname: tunnelTarget(tunnel.ID),
		Token:    token,
	}, nil
}

// checkExistingRecord accepts a pre-existing record only if it is exactly what
// CreateDNSRecord would have made: a proxied CNAME to the tunnel. A DNS-only
// CNAME to the tunnel does not carry traffic, so it must not count as ready.
func checkExistingRecord(rec cloudflare.DNSRecord, hostname, tunnelID string) error {
	if rec.Type != "CNAME" || rec.Content != tunnelTarget(tunnelID) {
		return fmt.Errorf("DNS record %q already exists (%s -> %s) and does not point at tunnel %s", hostname, rec.Type, rec.Content, tunnelID)
	}
	if rec.Proxied == nil || !*rec.Proxied {
		return fmt.Errorf("DNS record %q points at tunnel %s but is not proxied, so traffic would not reach the tunnel; enable the proxy or delete the record", hostname, tunnelID)
	}
	return nil
}

// tunnelTarget is the CNAME target Cloudflare assigns to a tunnel.
func tunnelTarget(tunnelID string) string {
	return fmt.Sprintf("%s.cfargotunnel.com", tunnelID)
}

// CreateDNSRecord is idempotent: if the CNAME already points at the tunnel it
// succeeds without changes, and if the name is taken by anything else it fails
// instead of overwriting a record the operator does not own.
func (c *RealCloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error {
	target := tunnelTarget(tunnelID)

	existing, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.ListDNSRecordsParams{Name: hostname})
	if err != nil {
		return fmt.Errorf("failed to list DNS records for %q: %w", hostname, err)
	}
	if len(existing) > 0 {
		return checkExistingRecord(existing[0], hostname, tunnelID)
	}

	_, err = c.api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.CreateDNSRecordParams{
		Type:    "CNAME",
		Name:    hostname,
		Content: target,
		Proxied: cloudflare.BoolPtr(true),
		TTL:     1,
	})
	if err != nil {
		return fmt.Errorf("failed to create Cloudflare DNS CNAME record: %w", err)
	}
	return nil
}

func (c *RealCloudflareClient) CreateAccessPolicy(ctx context.Context, accountID, name, domain string, allowedEmails []string) error {
	app, err := c.api.CreateAccessApplication(ctx, cloudflare.AccountIdentifier(accountID), cloudflare.CreateAccessApplicationParams{
		Name:   name,
		Domain: domain,
	})
	if err != nil {
		return fmt.Errorf("failed to create Access Application: %w", err)
	}

	var includeRules []interface{}
	for _, email := range allowedEmails {
		includeRules = append(includeRules, cloudflare.AccessGroupEmail{Email: struct {
			Email string `json:"email"`
		}{Email: email}})
	}

	_, err = c.api.CreateAccessPolicy(ctx, cloudflare.AccountIdentifier(accountID), cloudflare.CreateAccessPolicyParams{
		ApplicationID: app.ID,
		Name:          fmt.Sprintf("%s-policy", name),
		Decision:      "allow",
		Include:       includeRules,
	})
	if err != nil {
		return fmt.Errorf("failed to attach Access Policy: %w", err)
	}

	return nil
}

// DeleteTunnel treats "already gone" as success so cleanup converges when a
// tunnel was removed outside the operator. Any other failure (rate limits,
// 5xx, auth) is returned so the caller retries.
func (c *RealCloudflareClient) DeleteTunnel(ctx context.Context, tunnelID string) error {
	err := c.api.DeleteTunnel(ctx, cloudflare.AccountIdentifier(c.AccountID), tunnelID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete Cloudflare tunnel: %w", err)
	}
	return nil
}

// isNotFound reports whether err is a Cloudflare HTTP 404. The SDK surfaces
// 404s as *cloudflare.NotFoundError; the *cloudflare.Error check also covers
// the base type.
func isNotFound(err error) bool {
	var nf *cloudflare.NotFoundError
	if errors.As(err, &nf) {
		return true
	}
	var apiErr *cloudflare.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// DeleteDNSRecord deletes the CNAME records for hostname that point at
// tunnelID. Errors are returned, never swallowed: callers rely on them to retry.
func (c *RealCloudflareClient) DeleteDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error {
	target := tunnelTarget(tunnelID)

	records, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.ListDNSRecordsParams{Name: hostname})
	if err != nil {
		return fmt.Errorf("failed to list DNS records for %q: %w", hostname, err)
	}

	var errs []error
	for _, rec := range records {
		if rec.Type != "CNAME" || rec.Content != target {
			continue
		}
		if err := c.api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), rec.ID); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete DNS record %s: %w", rec.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (c *RealCloudflareClient) DeleteAccessPolicy(ctx context.Context, accountID, domain string) error {
	apps, _, err := c.api.ListAccessApplications(ctx, cloudflare.AccountIdentifier(accountID), cloudflare.ListAccessApplicationsParams{})
	if err != nil {
		return err
	}

	for _, app := range apps {
		if app.Domain == domain {
			return c.api.DeleteAccessApplication(ctx, cloudflare.AccountIdentifier(accountID), app.ID)
		}
	}
	return nil
}
