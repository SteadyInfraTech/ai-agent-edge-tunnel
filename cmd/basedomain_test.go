package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidateBaseDomain checks: which base domains are accepted at startup.
func TestValidateBaseDomain(t *testing.T) {
	label := strings.Repeat("a", 63)
	long := strings.Join([]string{label, label, label, "abc"}, ".") // 195 characters
	for name, tc := range map[string]struct {
		in string
		ok bool
	}{
		"plain":            {"agents.example.com", true},
		"leading dot":      {".agents.example.com", true},
		"scheme":           {"https://agents.example.com", false},
		"uppercase":        {"Agents.Example.com", false},
		"trailing dot":     {"agents.example.com.", false},
		"empty":            {"", false},
		"too long":         {long, false},
		"longest accepted": {strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 61), true},
		"underscore":       {"agents_x.example.com", false},
		"spaces":           {"agents example.com", false},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateBaseDomain(tc.in)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// TestValidateBaseDomainInZone rejects hostnames outside the configured zone.
func TestValidateBaseDomainInZone(t *testing.T) {
	for name, tc := range map[string]struct {
		base string
		zone string
		ok   bool
	}{
		"zone apex":        {"example.com", "example.com", true},
		"subdomain":        {"agents.example.com", "example.com", true},
		"nested subdomain": {"agents.dev.example.com", "example.com", true},
		"sibling domain":   {"example.net", "example.com", false},
		"suffix spoof":     {"notexample.com", "example.com", false},
		"invalid zone":     {"agents.example.com", "https://example.com", false},
		"empty zone":       {"agents.example.com", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateBaseDomainInZone(tc.base, tc.zone)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
