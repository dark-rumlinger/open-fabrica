package tlsproxy

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
)

// newProbeClient builds the HTTPS self-probe client. Any cert is accepted -
// the probe only needs to know the TLS handshake completes, not that the cert
// validates.
func newProbeClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- self-probe against our own listener; we only need to know the TLS handshake completes, not that the cert validates
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// checkHTTPSHealth waits `delay` (long enough for ACME issuance to finish
// in the typical case), then probes HTTPS for every published project domain.
// Each result is logged with an actionable hint and recorded in the status
// registry the admin hosting-status endpoint reads.
func (m *Manager) checkHTTPSHealth(delay time.Duration) {
	time.Sleep(delay)

	projects, err := models.ListPublishedProjects(m.db)
	if err != nil {
		return
	}

	client := newProbeClient()
	for _, project := range projects {
		if project.Domain == nil || *project.Domain == "" {
			continue
		}
		domain := *project.Domain
		// *.localhost and IP-literal domains are HTTP-only by design (no
		// publicly-trusted cert is ever issued for them - see isLocalOrIPDomain),
		// so probing them over HTTPS always times out. Skip them instead of
		// logging a spurious "HTTPS is not working" warning for every project.
		if isLocalOrIPDomain(domain) {
			continue
		}
		m.probeDomain(client, domain, project.ID)
	}
}

// issuanceGrace is how long after a domain enters the managed set a failed
// self-probe still counts as "not yet" rather than "broken". ACME regularly
// takes 30s+ from cold and the first health probe runs 20s in, so without this
// every freshly added domain would flash a failure before it ever had a chance.
const issuanceGrace = 2 * time.Minute

// verdictForFailedProbe turns a failed self-dial into a status. The dial leaves
// and re-enters this machine through its own router, so failure has three very
// different meanings and only the last is a real problem:
//
//   - a certificate is loaded: issuance worked and :443 is served, so this is
//     almost always a router without NAT loopback. Unverifiable from here.
//   - still inside issuanceGrace: ACME has not finished yet.
//   - neither: something is genuinely wrong, and Hint names the likely cause.
func (m *Manager) verdictForFailedProbe(domain string, err error, now time.Time) DomainStatus {
	if m.certLoadedFor(domain) {
		return DomainStatus{
			State:     StateUnverified,
			CheckedAt: &now,
			Hint:      fmt.Sprintf(msgcat.Mark("The certificate for '%s' is in place, but this computer cannot reach the site through your own router. Many routers work that way, and it says nothing about visitors. Open the site on a phone with Wi-Fi off to confirm."), domain),
		}
	}
	if since, ok := m.managedSinceFor(domain); ok && now.Sub(since) < issuanceGrace {
		return DomainStatus{State: StatePending, CheckedAt: &now}
	}
	return DomainStatus{State: StateFailed, CheckedAt: &now, Error: err.Error(), Hint: diagnoseHTTPSError(err, domain)}
}

// logProbeFailure logs at a level that matches the verdict: only a real failure
// is a warning. Pending and unverified are expected states, and logging them as
// problems trains the operator to ignore the log.
func (m *Manager) logProbeFailure(domain string, projectID int, st DomainStatus) {
	attrs := []any{"domain", domain}
	if projectID > 0 {
		attrs = append(attrs, "project_id", projectID)
	}
	if st.Hint != "" {
		attrs = append(attrs, "help", st.Hint)
	}
	switch st.State {
	case StatePending:
		m.logger.Debug("HTTPS not answering yet, certificate issuance still in progress", attrs...)
	case StateUnverified:
		m.logger.Info("HTTPS certificate is in place, but this host cannot reach the site through its own router", attrs...)
	default:
		m.logger.Warn("HTTPS is not working for this domain", attrs...)
	}
}

// probeDomain GETs https://{domain}/ once, logs the outcome, and records it in
// the status registry. projectID 0 means an on-demand probe with no project
// context for the log line.
func (m *Manager) probeDomain(client *http.Client, domain string, projectID int) DomainStatus {
	resp, err := client.Get(fmt.Sprintf("https://%s/", domain))
	now := time.Now()
	if err != nil {
		st := m.verdictForFailedProbe(domain, err, now)
		m.logProbeFailure(domain, projectID, st)
		m.setStatus(domain, st)
		return st
	}
	resp.Body.Close()
	if projectID > 0 {
		m.logger.Info("HTTPS is working",
			"domain", domain,
			"project_id", projectID,
			"status", resp.StatusCode,
		)
	} else {
		m.logger.Info("HTTPS is working",
			"domain", domain,
			"status", resp.StatusCode,
		)
	}
	st := DomainStatus{State: StateActive, CheckedAt: &now}
	m.setStatus(domain, st)
	return st
}

func diagnoseHTTPSError(err error, domain string) string {
	msg := strings.ToLower(err.Error())
	switch {
	case containsAny(msg, "no such host", "server mismatch"):
		return fmt.Sprintf(msgcat.Mark("DNS is not pointing '%s' to this server. Update your DNS A record to point to this server's public IP"), domain)
	case containsAny(msg, "connection refused"):
		return fmt.Sprintf(msgcat.Mark("port 443 is not reachable on '%s'. Make sure the port is open in your firewall and no other program is using it"), domain)
	case containsAny(msg, "timeout", "deadline exceeded"):
		return fmt.Sprintf(msgcat.Mark("connection to '%s' timed out. The domain may not point to this server, or a firewall is blocking port 443"), domain)
	case containsAny(msg, "certificate"):
		return fmt.Sprintf(msgcat.Mark("TLS certificate problem for '%s'. Cert issuance from Let's Encrypt can take 30+ seconds on first request; if this persists, check that ports 80 and 443 are open"), domain)
	default:
		return fmt.Sprintf(msgcat.Mark("could not connect to '%s' via HTTPS: %v"), domain, err)
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
