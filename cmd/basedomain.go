package main

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// maxBaseDomainLen leaves room for a 63-character label and a dot within the
// 253-character DNS name limit.
const maxBaseDomainLen = 253 - 63 - 1

// validateBaseDomain rejects a base domain that could never produce a valid
// hostname, so a typo fails at startup instead of at every provisioning. The
// controller trims one leading dot, so it is ignored here too.
func validateBaseDomain(d string) error {
	d = strings.TrimPrefix(d, ".")
	if len(d) > maxBaseDomainLen {
		return fmt.Errorf("base domain is %d characters; the maximum is %d", len(d), maxBaseDomainLen)
	}
	if errs := validation.IsDNS1123Subdomain(d); len(errs) > 0 {
		return fmt.Errorf("invalid base domain %q: %s", d, strings.Join(errs, "; "))
	}
	return nil
}

// validateBaseDomainInZone ensures generated hostnames stay inside the
// Cloudflare zone configured alongside its ID. The ID/name pair is an
// administrator-supplied trust anchor; DNS operations still use the pinned ID.
func validateBaseDomainInZone(baseDomain, zoneName string) error {
	baseDomain = strings.TrimPrefix(baseDomain, ".")
	zoneName = strings.TrimPrefix(zoneName, ".")
	if err := validateBaseDomain(zoneName); err != nil {
		return fmt.Errorf("invalid Cloudflare zone name %q: %w", zoneName, err)
	}
	if err := validateBaseDomain(baseDomain); err != nil {
		return err
	}
	if baseDomain != zoneName && !strings.HasSuffix(baseDomain, "."+zoneName) {
		return fmt.Errorf("base domain %q is not the configured Cloudflare zone %q or one of its subdomains", baseDomain, zoneName)
	}
	return nil
}
