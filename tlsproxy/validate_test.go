package tlsproxy

import (
	"strings"
	"testing"
)

// TestValidateDomainRejectsMalformed covers the format checks that fail before
// any DNS lookup, keeping the test hermetic (no network).
func TestValidateDomainRejectsMalformed(t *testing.T) {
	cases := []struct {
		domain   string
		errMatch string
	}{
		{"", "empty"},
		{"has space.com", "spaces"},
		{"http://example.com", "protocol"},
		{"https://example.com", "protocol"},
		{"example.com/path", "path"},
	}
	for _, c := range cases {
		r := ValidateDomain(c.domain)
		if r.Valid {
			t.Errorf("ValidateDomain(%q) unexpectedly valid", c.domain)
			continue
		}
		if !strings.Contains(r.Error, c.errMatch) {
			t.Errorf("ValidateDomain(%q) error = %q, want it to contain %q", c.domain, r.Error, c.errMatch)
		}
	}
}

// TestValidateDomainLocalIsValidButHTTPOnly verifies loopback names and IP
// literals are accepted for routing but flagged as HTTPS-ineligible with a
// warning. These branches also avoid DNS.
func TestValidateDomainLocalIsValidButHTTPOnly(t *testing.T) {
	for _, d := range []string{"localhost", "app.localhost", "10.0.0.5"} {
		r := ValidateDomain(d)
		if !r.Valid {
			t.Errorf("ValidateDomain(%q) should be valid (routes over plain HTTP)", d)
		}
		if r.HTTPSAvailable {
			t.Errorf("ValidateDomain(%q) should report HTTPSAvailable=false", d)
		}
		if len(r.Warnings) == 0 {
			t.Errorf("ValidateDomain(%q) should carry a warning explaining HTTP-only", d)
		}
	}
}

// TestFormatCheckResult covers the human-readable summary for the two
// network-free outcomes: a fatal format error, and a valid-but-HTTP-only host.
func TestFormatCheckResult(t *testing.T) {
	if got := ValidateDomain("").FormatCheckResult(); !strings.Contains(got, "will fail") {
		t.Errorf("invalid domain summary = %q, want it to mention failure", got)
	}
	if got := ValidateDomain("localhost").FormatCheckResult(); !strings.Contains(got, "HTTP") {
		t.Errorf("local domain summary = %q, want it to mention HTTP", got)
	}
}
