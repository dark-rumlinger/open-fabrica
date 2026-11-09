package tlsproxy

import (
	"net"
	"strings"
	"time"
)

// Domain HTTPS health states as reported by Status and Probe.
//
// The self-probe dials our own public hostname from the machine that serves it,
// so a failed dial is weak evidence on its own: it means "broken" only after
// issuance has had time to finish AND no certificate is loaded. The other two
// readings of the same failure get their own states so no surface paints them
// red. See verdictForFailedProbe.
const (
	StatePending    = "pending"    // managed, no successful probe recorded yet
	StateActive     = "active"     // last probe completed a TLS handshake
	StateUnverified = "unverified" // certificate is loaded, but this host cannot reach the site through its own router (no NAT loopback); only a device off this network can settle it
	StateFailed     = "failed"     // last probe failed past the issuance grace with no cert loaded; Hint explains the likely fix
)

// DomainStatus is the recorded HTTPS health for one domain, decorated on read
// with live certificate presence/expiry from the certmagic cache. It is the
// payload behind the admin hosting-status endpoint, so every field is
// JSON-shaped for the UI.
type DomainStatus struct {
	State      string     `json:"state"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Hint       string     `json:"hint,omitempty"`
	CertLoaded bool       `json:"cert_loaded"`
	CertExpiry *time.Time `json:"cert_expiry,omitempty"`
}

// DomainKindNone etc. classify a project's domain value for the admin UI, which
// words its guidance differently for each kind.
const (
	DomainKindNone   = "none"   // no domain configured
	DomainKindLocal  = "local"  // localhost / *.localhost, reachable only on this machine
	DomainKindIP     = "ip"     // IP literal, plain HTTP on the public port
	DomainKindPublic = "public" // real domain name, eligible for HTTPS
)

// DomainKind classifies a domain value into one of the DomainKind* constants.
func DomainKind(domain string) string {
	switch {
	case domain == "":
		return DomainKindNone
	case domain == "localhost" || strings.HasSuffix(domain, ".localhost"):
		return DomainKindLocal
	case net.ParseIP(domain) != nil:
		return DomainKindIP
	default:
		return DomainKindPublic
	}
}

// setStatus records a probe outcome for a domain. Cert fields are computed on
// read in Status, so recorded entries carry only probe results.
func (m *Manager) setStatus(domain string, st DomainStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[domain] = st
}

// Status returns the recorded HTTPS health for a domain plus live certificate
// presence/expiry from the certmagic cache. A domain with no recorded probe
// reports pending: the health self-probe runs ~20s after issuance starts, and
// Probe offers an on-demand check.
func (m *Manager) Status(domain string) DomainStatus {
	m.mu.Lock()
	st, ok := m.statuses[domain]
	m.mu.Unlock()
	if !ok {
		st = DomainStatus{State: StatePending}
	}
	if m.cache != nil {
		for _, cert := range m.cache.AllMatchingCertificates(domain) {
			if cert.Leaf == nil {
				continue
			}
			st.CertLoaded = true
			exp := cert.Leaf.NotAfter
			if st.CertExpiry == nil || exp.After(*st.CertExpiry) {
				expiry := exp
				st.CertExpiry = &expiry
			}
		}
	}
	return st
}

// certLoadedFor reports whether the certmagic cache holds a usable certificate
// for the domain. This is the difference between "issuance never worked" and
// "issuance worked but this host cannot dial itself through the router", so the
// probe verdict turns on it.
func (m *Manager) certLoadedFor(domain string) bool {
	if m.cache == nil {
		return false
	}
	for _, cert := range m.cache.AllMatchingCertificates(domain) {
		if cert.Leaf != nil {
			return true
		}
	}
	return false
}

// Probe runs an on-demand HTTPS check for one domain, records the result, and
// returns the updated status. Concurrent probes for the same domain collapse
// into one: a caller that finds a probe in flight gets the current recorded
// status immediately. Local and IP domains are never probed (HTTPS does not
// apply to them); their current status is returned as-is.
func (m *Manager) Probe(domain string) DomainStatus {
	if isLocalOrIPDomain(domain) {
		return m.Status(domain)
	}
	m.mu.Lock()
	if _, busy := m.probing[domain]; busy {
		m.mu.Unlock()
		return m.Status(domain)
	}
	m.probing[domain] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.probing, domain)
		m.mu.Unlock()
	}()

	m.probeDomain(newProbeClient(), domain, 0)
	return m.Status(domain)
}

// LastStartError returns the humanized bind failure from the most recent Start
// attempt, or "" when the listeners are bound (or Start has not been needed).
// The admin hosting-status endpoint surfaces it so a port conflict shows up in
// the UI instead of only in the log.
func (m *Manager) LastStartError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStartErr
}
