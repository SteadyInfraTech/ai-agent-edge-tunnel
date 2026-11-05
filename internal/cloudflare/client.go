package cloudflare

import (
	"context"
	"fmt"

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
	DeleteDNSRecord(ctx context.Context, zoneID, hostname string) error
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
	secret := "a-randomly-generated-32-byte-secret-key" // Replace with crypto/rand token generation

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
		Hostname: fmt.Sprintf("%s.cfargotunnel.com", tunnel.ID),
		Token:    token,
	}, nil
}

func (c *RealCloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error {
	target := fmt.Sprintf("%s.cfargotunnel.com", tunnelID)

	_, err := c.api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.CreateDNSRecordParams{
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
	app, err := c.api.CreateAccessApplication(ctx, cloudflare.AccountIdentifier(accountID), cloudflare.AccessApplication{
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

	_, err = c.api.CreateAccessPolicy(ctx, cloudflare.AccountIdentifier(accountID), cloudflare.AccessPolicy{
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

func (c *RealCloudflareClient) DeleteTunnel(ctx context.Context, tunnelID string) error {
	err := c.api.DeleteTunnel(ctx, cloudflare.AccountIdentifier(c.AccountID), tunnelID)
	if err != nil {
		return fmt.Errorf("failed to delete Cloudflare tunnel: %w", err)
	}
	return nil
}

func (c *RealCloudflareClient) DeleteDNSRecord(ctx context.Context, zoneID, hostname string) error {
	records, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.ListDNSRecordsParams{Name: hostname})
	if err != nil || len(records) == 0 {
		return nil
	}
	return c.api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), records[0].ID)
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
