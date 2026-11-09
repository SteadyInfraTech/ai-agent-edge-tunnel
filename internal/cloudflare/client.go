package cloudflare

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudflare/cloudflare-go"
)

// DryRunIDPrefix starts every ID handed out by the in-memory fake. It lets the
// controller tell state written by --dry-run apart from state that refers to
// real Cloudflare resources, so neither mode acts on the other's objects.
const DryRunIDPrefix = "dryrun-"

// IsDryRunID reports whether id was issued by the in-memory fake.
func IsDryRunID(id string) bool { return strings.HasPrefix(id, DryRunIDPrefix) }

// TunnelInfo describes a tunnel as returned by Cloudflare. The secret is
// generated before creation and stored immediately in the connector Secret.
type TunnelInfo struct {
	// AccountID is the Cloudflare account that owns the tunnel.
	AccountID string
	ID        string
	Name      string
	Secret    string
	Hostname  string
}

// TunnelRef identifies an existing tunnel.
type TunnelRef struct {
	ID   string
	Name string
}

// AccessAppRef identifies an existing Access application.
type AccessAppRef struct {
	ID string
	// HasAllowPolicy is true when the application has at least one allow
	// policy. An application without one lets nobody in, so it is a leftover
	// from an attempt that stopped before attaching the policy.
	HasAllowPolicy bool
}

type TunnelService interface {
	CreateTunnel(ctx context.Context, name string) (*TunnelInfo, error)
	// FindTunnels returns the tunnels that exist (not deleted) with exactly this
	// name. The name is only a lookup key; the controller must establish ownership
	// independently before adopting or deleting a match.
	FindTunnels(ctx context.Context, name string) ([]TunnelRef, error)
	// FindAccessApplications returns the Access applications of accountID whose
	// name and domain both equal the given values.
	FindAccessApplications(ctx context.Context, accountID, name, domain string) ([]AccessAppRef, error)
	// VerifyAccessPolicy checks that the recorded application still has exactly the intended allow policy.
	VerifyAccessPolicy(ctx context.Context, accountID, appID, name, domain string, allowedEmails []string) (bool, error)
	CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error
	// CreateAccessPolicy creates an Access application for domain with an allow
	// policy for the given emails and returns the application ID. If the policy
	// cannot be attached, the application is removed again so nothing is left
	// behind that the caller does not know about.
	CreateAccessPolicy(ctx context.Context, accountID, name, domain string, allowedEmails []string) (appID string, err error)
	DeleteTunnel(ctx context.Context, tunnelID string) error
	// DeleteDNSRecord removes only CNAME records for hostname that point at
	// tunnelID, so it can never delete a record this operator did not create.
	DeleteDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error
	// DeleteAccessApplication deletes by ID, never by domain, so it cannot
	// remove an application this operator did not create. Already gone is success.
	DeleteAccessApplication(ctx context.Context, accountID, appID string) error
}

type RealCloudflareClient struct {
	AccountID string
	APIToken  string
	api       *cloudflare.API
}

// NewRealCloudflareClient returns a client that talks to the Cloudflare API
// with the given account ID and API token.
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

// CreateTunnel creates a locally managed tunnel and returns its ID and
// credentials. The secret is generated locally and stored only in the connector Secret.
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
	return &TunnelInfo{
		AccountID: c.AccountID,
		ID:        tunnel.ID,
		Name:      tunnel.Name,
		Secret:    secret,
		Hostname:  tunnelTarget(tunnel.ID),
	}, nil
}

// selectCNAME returns the CNAME among records, and whether there is one. Other
// record types at the same name are ignored: Cloudflare allows a TXT record
// beside a CNAME and does not guarantee list order, so looking only at the first
// record could reject a hostname that already has the right CNAME. A CNAME that
// points at tunnelID is preferred over any other.
func selectCNAME(records []cloudflare.DNSRecord, tunnelID string) (cloudflare.DNSRecord, bool) {
	target := tunnelTarget(tunnelID)
	var other *cloudflare.DNSRecord
	for i := range records {
		if records[i].Type != "CNAME" {
			continue
		}
		if records[i].Content == target {
			return records[i], true
		}
		if other == nil {
			other = &records[i]
		}
	}
	if other != nil {
		return *other, true
	}
	return cloudflare.DNSRecord{}, false
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

// CreateDNSRecord is idempotent: if a proxied CNAME already points at the tunnel
// it succeeds without changes, and if the name has a CNAME for anything else it
// fails instead of overwriting a record the operator does not own. Other record
// types at the name (such as TXT) are left alone.
func (c *RealCloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) error {
	target := tunnelTarget(tunnelID)

	existing, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.ListDNSRecordsParams{Name: hostname})
	if err != nil {
		return fmt.Errorf("failed to list DNS records for %q: %w", hostname, err)
	}
	if rec, ok := selectCNAME(existing, tunnelID); ok {
		return checkExistingRecord(rec, hostname, tunnelID)
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

// CreateAccessPolicy creates an Access application for domain with an allow
// policy for allowedEmails, and returns the application ID.
func (c *RealCloudflareClient) CreateAccessPolicy(ctx context.Context, accountID, name, domain string, allowedEmails []string) (string, error) {
	app, err := c.api.CreateAccessApplication(ctx, cloudflare.AccountIdentifier(accountID), cloudflare.CreateAccessApplicationParams{
		Name:   name,
		Domain: domain,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create Access Application: %w", err)
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
		// An application without a policy would be left behind, unknown to the
		// caller. Remove it, and report both failures if cleanup also fails.
		if delErr := c.DeleteAccessApplication(ctx, accountID, app.ID); delErr != nil {
			return "", errors.Join(fmt.Errorf("failed to attach Access Policy: %w", err), delErr)
		}
		return "", fmt.Errorf("failed to attach Access Policy: %w", err)
	}

	return app.ID, nil
}

// DeleteAccessApplication deletes the Access application with the given ID. An
// application that is already gone is not an error.
func (c *RealCloudflareClient) DeleteAccessApplication(ctx context.Context, accountID, appID string) error {
	err := c.api.DeleteAccessApplication(ctx, cloudflare.AccountIdentifier(accountID), appID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete Access Application %s: %w", appID, err)
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

// FindTunnels lists the tunnels named exactly name that are not deleted.
func (c *RealCloudflareClient) FindTunnels(ctx context.Context, name string) ([]TunnelRef, error) {
	var out []TunnelRef
	params := cloudflare.TunnelListParams{
		Name:       name,
		IsDeleted:  cloudflare.BoolPtr(false),
		ResultInfo: cloudflare.ResultInfo{PerPage: 100, Page: 1},
	}
	for {
		tunnels, info, err := c.api.ListTunnels(ctx, cloudflare.AccountIdentifier(c.AccountID), params)
		if err != nil {
			return nil, fmt.Errorf("failed to list Cloudflare tunnels named %q: %w", name, err)
		}
		for _, t := range tunnels {
			// The filter is applied by Cloudflare; checking again means a
			// changed filter semantic can never make us act on someone else's tunnel.
			if t.Name == name && t.DeletedAt == nil {
				out = append(out, TunnelRef{ID: t.ID, Name: t.Name})
			}
		}
		if info == nil || !info.HasMorePages() {
			return out, nil
		}
		params.ResultInfo = info.Next()
	}
}

// VerifyAccessPolicy verifies the recorded application and its complete policy surface.
// It fails closed on extra policies, non-email selectors, exclusions, requirements,
// or any change to the allowlist. API errors are returned separately from drift.
func (c *RealCloudflareClient) VerifyAccessPolicy(ctx context.Context, accountID, appID, name, domain string, allowedEmails []string) (bool, error) {
	rc := cloudflare.AccountIdentifier(accountID)
	app, err := c.api.GetAccessApplication(ctx, rc, appID)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting Access application %s: %w", appID, err)
	}
	if app.ID != appID || app.Name != name || app.Domain != domain {
		return false, nil
	}
	policies, _, err := c.api.ListAccessPolicies(ctx, rc, cloudflare.ListAccessPoliciesParams{ApplicationID: appID})
	if err != nil {
		return false, fmt.Errorf("listing policies for Access application %s: %w", appID, err)
	}
	if len(policies) != 1 {
		return false, nil
	}
	policy := policies[0]
	if policy.Decision != "allow" || len(policy.Include) != len(allowedEmails) || len(policy.Exclude) != 0 || len(policy.Require) != 0 {
		return false, nil
	}
	want := make(map[string]struct{}, len(allowedEmails))
	for _, email := range allowedEmails {
		want[strings.ToLower(strings.TrimSpace(email))] = struct{}{}
	}
	if len(want) != len(allowedEmails) {
		return false, nil
	}
	for _, raw := range policy.Include {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return false, fmt.Errorf("encoding Access policy selector: %w", err)
		}
		var selector map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &selector); err != nil || len(selector) != 1 {
			return false, nil
		}
		emailRaw, ok := selector["email"]
		if !ok {
			return false, nil
		}
		var emailSelector map[string]string
		if err := json.Unmarshal(emailRaw, &emailSelector); err != nil || len(emailSelector) != 1 {
			return false, nil
		}
		email := strings.ToLower(strings.TrimSpace(emailSelector["email"]))
		if _, ok := want[email]; !ok {
			return false, nil
		}
		delete(want, email)
	}
	return len(want) == 0, nil
}

// FindAccessApplications lists the Access applications whose name and domain
// both match, and whether each has an allow policy.
func (c *RealCloudflareClient) FindAccessApplications(ctx context.Context, accountID, name, domain string) ([]AccessAppRef, error) {
	var out []AccessAppRef
	rc := cloudflare.AccountIdentifier(accountID)
	params := cloudflare.ListAccessApplicationsParams{ResultInfo: cloudflare.ResultInfo{PerPage: 100, Page: 1}}
	for {
		apps, info, err := c.api.ListAccessApplications(ctx, rc, params)
		if err != nil {
			return nil, fmt.Errorf("failed to list Access applications: %w", err)
		}
		for _, app := range apps {
			if app.Name != name || app.Domain != domain {
				continue
			}
			policies, _, err := c.api.ListAccessPolicies(ctx, rc, cloudflare.ListAccessPoliciesParams{ApplicationID: app.ID})
			if err != nil {
				return nil, fmt.Errorf("failed to list policies of Access application %s: %w", app.ID, err)
			}
			ref := AccessAppRef{ID: app.ID}
			for _, pol := range policies {
				if pol.Decision == "allow" && len(pol.Include) > 0 {
					ref.HasAllowPolicy = true
				}
			}
			out = append(out, ref)
		}
		if info == nil || !info.HasMorePages() {
			return out, nil
		}
		params.ResultInfo = info.Next()
	}
}
