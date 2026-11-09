package tlsproxy

import (
	"fmt"
	"net"
	"strings"

	"openfabrica/internal/msgcat"
)

// DomainCheck holds the result of pre-flight domain validation.
type DomainCheck struct {
	Domain   string
	Valid    bool
	Warnings []string // non-fatal issues (e.g. slow DNS)
	Error    string   // fatal issue preventing HTTPS
	// HTTPSAvailable is false for loopback names and IP literals. These route
	// fine over plain HTTP on the public port, but no publicly-trusted
	// certificate can ever be issued for them (Let's Encrypt rejects IPs and
	// *.localhost). The TLS manager uses this to keep such domains out of the
	// ACME-managed set so it doesn't spin on doomed orders. The domain is still
	// Valid - routing works, just not HTTPS.
	HTTPSAvailable bool
}

// isLocalOrIPDomain reports whether the domain is a loopback name (localhost,
// *.localhost) or an IP literal. Such hosts are reachable over plain HTTP but
// are ineligible for a Let's Encrypt certificate.
func isLocalOrIPDomain(domain string) bool {
	if domain == "localhost" || strings.HasSuffix(domain, ".localhost") {
		return true
	}
	return net.ParseIP(domain) != nil
}

// ValidateDomain runs pre-flight checks on a domain before adding it to
// the active managed set. Catches common misconfigurations early so the
// user sees a clear message instead of a wall of ACME errors.
func ValidateDomain(domain string) *DomainCheck {
	result := &DomainCheck{Domain: domain, Valid: true, HTTPSAvailable: true}

	// Basic format checks.
	if domain == "" {
		result.Valid = false
		result.Error = msgcat.Mark("domain is empty")
		return result
	}
	if strings.Contains(domain, " ") {
		result.Valid = false
		result.Error = fmt.Sprintf(msgcat.Mark("domain '%s' contains spaces"), domain)
		return result
	}
	if strings.Contains(domain, "://") {
		result.Valid = false
		result.Error = fmt.Sprintf(msgcat.Mark("domain should not include protocol (remove http:// or https:// from '%s')"), domain)
		return result
	}
	if strings.Contains(domain, "/") {
		result.Valid = false
		result.Error = fmt.Sprintf(msgcat.Mark("domain should not include a path (remove everything after the domain name in '%s')"), domain)
		return result
	}

	// Loopback names and IP addresses can't get ACME certificates. Accept the
	// domain (it routes fine over plain HTTP) but flag HTTPS as unavailable so
	// the TLS manager won't add it to the ACME-managed set. There is no
	// self-signed fallback configured, so HTTPS simply isn't offered for these.
	if isLocalOrIPDomain(domain) {
		result.HTTPSAvailable = false
		result.Warnings = append(result.Warnings, fmt.Sprintf(msgcat.Mark("'%s' is a local or IP address. It is served over plain HTTP on the public port (default :5733); HTTPS is not available, because a publicly-trusted certificate can only be issued for a real domain name. Use a domain name if you need HTTPS"), domain))
		return result
	}

	// DNS resolution check.
	resolved, err := net.LookupHost(domain)
	if err != nil {
		result.Valid = false
		dnsErr, ok := err.(*net.DNSError)
		switch {
		case ok && dnsErr.IsNotFound:
			result.Error = fmt.Sprintf(msgcat.Mark("domain '%s' does not resolve to any IP address. Make sure your DNS A/AAAA record points to this server's IP"), domain)
		case ok && dnsErr.IsTimeout:
			result.Error = fmt.Sprintf(msgcat.Mark("DNS lookup for '%s' timed out. Check your DNS configuration or try again"), domain)
		default:
			result.Error = fmt.Sprintf(msgcat.Mark("DNS lookup for '%s' failed: %v. Make sure the domain's DNS is configured correctly"), domain, err)
		}
		return result
	}

	hasRoutable := false
	for _, ip := range resolved {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		if !parsed.IsLoopback() && !parsed.IsUnspecified() {
			hasRoutable = true
		}
	}
	if !hasRoutable && len(resolved) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(msgcat.Mark("domain '%s' resolves to %v which appears to be a loopback address. Let's Encrypt needs the domain to point to a public IP"), domain, resolved))
	}

	// Port reachability is intentionally not probed here: dialing :80/:443
	// would only test the local host (and dialing the domain's own IP from this
	// server hairpins unreliably), so any result is misleading. A genuine
	// reachability problem surfaces through the ACME challenge itself.

	return result
}

// FormatCheckResult returns a human-readable summary of the domain check.
func (c *DomainCheck) FormatCheckResult() string {
	if !c.Valid {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("HTTPS setup for '%s' will fail: %s", c.Domain, c.Error))
		for _, w := range c.Warnings {
			b.WriteString("\n  * ")
			b.WriteString(w)
		}
		return b.String()
	}
	// Valid but HTTP-only (loopback/IP): the warning already carries the full
	// explanation, so surface it directly instead of the DNS-check phrasing.
	if !c.HTTPSAvailable && len(c.Warnings) > 0 {
		return c.Warnings[0]
	}
	if len(c.Warnings) == 0 {
		return fmt.Sprintf("domain '%s' looks good", c.Domain)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("domain '%s' passed DNS check but has warnings:", c.Domain))
	for _, w := range c.Warnings {
		b.WriteString("\n  * ")
		b.WriteString(w)
	}
	return b.String()
}
