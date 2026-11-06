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
