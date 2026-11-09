// Package tlsproxy terminates HTTPS for project-owned custom domains, using
// certmagic for Let's Encrypt issuance, renewal, OCSP stapling, and on-demand TLS.
//
// The manager is driven by DB state, not a config flag: if any active project has
// a Domain set, it binds :80/:443 and manages certs for those domains. New domains
// added at runtime via the admin API trigger ManageAsync calls through the event
// bus, so adding a domain never requires a restart or config edit. All :443
// traffic is reverse-proxied to localhost:{PublicPort}, where the ProjectResolver
// middleware dispatches by Host header.
package tlsproxy

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"

	"openfabrica/config"
	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/safego"
)

// Manager owns the certmagic.Config + the :80 and :443 servers. Lifecycle
// is lazy: NewManager allocates state only; Start() binds ports and kicks
// off ACME. The event-bus subscription auto-starts the manager the first
// time a project gains a domain, so the admin UI flow never requires
// a restart.
type Manager struct {
	config *config.Config
	db     *sql.DB
	logger *slog.Logger

	magic *certmagic.Config
	cache *certmagic.Cache

	mu       sync.Mutex
	running  bool
	stopped  bool                 // Stop was called; the process is shutting down and must not re-bind
	managed  map[string]struct{}  // currently managed domains
	since    map[string]time.Time // when each domain entered the managed set, for the issuance grace
	httpsSrv *http.Server
	httpSrv  *http.Server

	// reconcileStop closes when the periodic domain reconcile should exit.
	reconcileStop chan struct{}

	// statuses records the last HTTPS probe result per domain; probing
	// collapses concurrent on-demand probes for the same domain; lastStartErr
	// keeps the humanized bind failure from the most recent Start attempt so
	// the admin hosting-status endpoint can surface it. See status.go.
	statuses     map[string]DomainStatus
	probing      map[string]struct{}
	lastStartErr string
}

// NewManager constructs a Manager without starting any listeners or issuing
// certificates. Safe to call even on hosts where ports 80/443 are unavailable.
func NewManager(cfg *config.Config, db *sql.DB, logger *slog.Logger) *Manager {
	m := &Manager{
		config:   cfg,
		db:       db,
		logger:   logger.With("component", "tlsproxy"),
		managed:  make(map[string]struct{}),
		since:    make(map[string]time.Time),
		statuses: make(map[string]DomainStatus),
		probing:  make(map[string]struct{}),
	}
	m.buildCertmagicConfig()
	return m
}

// buildCertmagicConfig wires a fresh certmagic.Config. We deliberately do
// not touch the package-level certmagic.Default singleton, which would
// prompt interactively for an email on first issuance. Email is left
// empty: Let's Encrypt no longer uses it for any purpose.
//
// The zap.NewNop() values below are the ONLY use of go.uber.org/zap in this
// codebase: certmagic's Logger fields are typed *zap.Logger, so we satisfy the
// interface with a no-op and log TLS lifecycle from our own slog wrapper
// (m.logger). Everything else in the module logs via log/slog - do not
// introduce zap elsewhere.
func (m *Manager) buildCertmagicConfig() {
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(_ certmagic.Certificate) (*certmagic.Config, error) {
			return m.magic, nil
		},
		// The Cache has its own logger, separate from Config.Logger. Without
		// this, certmagic prints "started background certificate maintenance"
		// at info level on every startup - noisy even when we never issue.
		Logger: zap.NewNop(),
	})
	m.cache = cache

	ca := certmagic.LetsEncryptProductionCA
	if m.config.TLSStaging {
		ca = certmagic.LetsEncryptStagingCA
		m.logger.Info("using Let's Encrypt STAGING CA - issued certs will not be trusted by browsers")
	}

	m.magic = certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{
			Path: filepath.Join(m.config.DataDir, "certs"),
		},
		// On-demand is a safety net: if a request arrives for a domain
		// the DB knows about but we haven't yet called ManageAsync for
		// (race between project create and event delivery), we still
		// issue. DecisionFunc gates this against the DB so random SNI
		// scans cannot trigger ACME abuse.
		OnDemand: &certmagic.OnDemandConfig{
			DecisionFunc: func(_ context.Context, name string) error {
				return m.allowDomain(name)
			},
		},
		Logger: zap.NewNop(), // we log lifecycle from our wrapper
	})

	issuer := certmagic.NewACMEIssuer(m.magic, certmagic.ACMEIssuer{
		CA:     ca,
		Email:  "", // LE no longer uses email for anything
		Agreed: true,
		Logger: zap.NewNop(),
	})
	m.magic.Issuers = []certmagic.Issuer{issuer}
}

// allowDomain returns nil if the domain belongs to a published project in
// the DB. Used both as the on-demand DecisionFunc and as a sanity check
// before ManageAsync.
func (m *Manager) allowDomain(name string) error {
	p, err := models.GetProjectByDomain(m.db, name)
	if err != nil {
		return fmt.Errorf("domain %q not registered to any project", name)
	}
	if !p.Published {
		return fmt.Errorf("project %d (%s) for domain %q is not published", p.ID, p.Name, name)
	}
	return nil
}

// IsRunning reports whether the HTTPS listener is bound. Used by main.go
// to gate JWT secure-cookie mode.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// StartIfDomainsExist checks the DB for active projects with a non-empty
// Domain. If at least one exists, it binds :80/:443 and starts cert
// management. If none, it returns nil without binding anything - event
// handlers will Start() it later when a domain is first added.
func (m *Manager) StartIfDomainsExist() error {
	domains, err := m.activeDomains()
	if err != nil {
		return fmt.Errorf("listing active project domains: %w", err)
	}
	// The reconcile loop runs whether or not there is anything to serve yet.
	// A domain added later arrives through the event bus, but a domain whose DNS
	// has not propagated is dropped by filterValidDomains and never reaches the
	// managed set - without a periodic pass it would stay invisible until someone
	// saved the project a second time, which is exactly the trap that made adding
	// a domain feel like it had failed.
	m.mu.Lock()
	m.startReconcileLoop()
	m.mu.Unlock()
	if len(domains) == 0 {
		m.logger.Info("no project domains configured, TLS listeners idle (will auto-start when a domain is added)")
		return nil
	}
	return m.Start()
}

// Start binds :80 and :443 and begins managing all currently configured
// project domains. Idempotent - safe to call when already running.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running || m.stopped {
		return nil
	}

	domains, err := m.activeDomainsLocked()
	if err != nil {
		return fmt.Errorf("listing active project domains: %w", err)
	}

	// Reverse proxy to the internal public HTTP server. Preserve the
	// original Host header so the public server's ProjectResolver
	// middleware can dispatch by hostname.
	target, _ := url.Parse(fmt.Sprintf("http://localhost:%d", m.config.PublicPort))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL points the outbound request at the internal public
			// server and clears Out.Host so it would route by the target
			// URL; we restore the original Host header afterwards so the
			// public server's ProjectResolver can still dispatch by hostname.
			originalHost := pr.In.Host
			pr.SetURL(target)
			pr.Out.Host = originalHost
			pr.Out.Header.Set("X-Forwarded-Host", originalHost)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			// Set the forwarded client IP from the real TCP peer and remove any
			// client-supplied forwarding/trust headers. The Rewrite hook (unlike
			// the older Director hook) does not append X-Forwarded-For, so without
			// this a client could spoof X-Forwarded-For / X-Real-IP (read by
			// middleware.ClientIP) straight through the proxy.
			clientIP := pr.In.RemoteAddr
			if host, _, err := net.SplitHostPort(clientIP); err == nil {
				clientIP = host
			}
			pr.Out.Header.Set("X-Forwarded-For", clientIP)
			pr.Out.Header.Del("X-Real-IP")
		},
	}
	// FlushInterval -1 means "flush immediately after every write". This is
	// belt-and-suspenders for streaming endpoints: WebSockets work via the
	// stdlib's Connection: Upgrade hijacking regardless, but SSE
	// (server/public/stream.go) and any other long-lived response benefit
	// from explicit flushing so chunks aren't held in the proxy buffer.
	proxy.FlushInterval = -1

	// HTTPS listener on :443. certmagic's TLSConfig handles SNI cert
	// selection automatically. We pin MinVersion explicitly so a future
	// certmagic default change can't silently lower our TLS floor.
	tlsConf := m.magic.TLSConfig()
	tlsConf.MinVersion = tls.VersionTLS12
	// No WriteTimeout and no ReadTimeout: this edge reverse-proxies long-lived SSE
	// and streaming responses, and large uploads, and a finite deadline on the
	// connection would truncate them (a ReadTimeout covers the whole request body,
	// so a big upload over a slow link died at 30 s here even though the project
	// server behind it extends its own deadline for upload routes). Slowloris is
	// bounded by ReadHeaderTimeout, idle keep-alive connections by IdleTimeout, and
	// the body by the project server's own read deadline, which fails the proxied
	// request when a client stalls.
	m.httpsSrv = &http.Server{
		Addr:              ":443",
		Handler:           proxy,
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	// HTTP listener on :80 serves ACME HTTP-01 challenges and redirects
	// everything else to HTTPS.
	acme := m.magic.Issuers[0].(*certmagic.ACMEIssuer)
	httpHandler := acme.HTTPChallengeHandler(http.HandlerFunc(redirectToHTTPS))
	m.httpSrv = &http.Server{
		Addr:              ":80",
		Handler:           httpHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Bind both ports before declaring success. If either fails, tear
	// down and report a clear actionable error.
	httpLn, err := bindWithDiag(":80")
	if err != nil {
		m.httpsSrv = nil
		m.httpSrv = nil
		m.lastStartErr = err.Error()
		return err
	}
	httpsLn, err := bindWithDiag(":443")
	if err != nil {
		_ = httpLn.Close()
		m.httpsSrv = nil
		m.httpSrv = nil
		m.lastStartErr = err.Error()
		return err
	}

	safego.Go("tlsproxy http listener", func() {
		if err := m.httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.logger.Error("HTTP server stopped unexpectedly", "error", err)
		}
	})
	safego.Go("tlsproxy https listener", func() {
		// ServeTLS with empty cert/key paths uses TLSConfig.GetCertificate
		// (set by certmagic.TLSConfig()) instead of files.
		if err := m.httpsSrv.ServeTLS(httpsLn, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.logger.Error("HTTPS server stopped unexpectedly", "error", err)
		}
	})

	m.running = true
	m.lastStartErr = ""
	m.logger.Info("TLS listeners bound", "domains", domains)

	// Kick off cert issuance asynchronously. Don't block Start() on it -
	// new certs can take 20s+ from cold and we want the public server
	// usable for HTTP-01 challenges immediately.
	if err := m.magic.ManageAsync(context.Background(), domains); err != nil {
		m.logger.Error("ManageAsync failed", "error", err)
	}
	now := time.Now()
	for _, d := range domains {
		m.managed[d] = struct{}{}
		m.since[d] = now
	}

	go m.checkHTTPSHealth(20 * time.Second)
	m.startReconcileLoop()
	return nil
}

// Stop gracefully shuts down both listeners. Idempotent.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Latch the shutdown and end the reconcile loop before the early return: the
	// loop runs even when the listeners never bound, and a tick landing during
	// shutdown would reload against a closing database. The latch also stops a
	// reconcile already inside Reload from seeing !running and helpfully
	// re-binding the ports we are in the middle of closing.
	m.stopped = true
	if m.reconcileStop != nil {
		close(m.reconcileStop)
		m.reconcileStop = nil
	}

	if !m.running {
		return nil
	}

	m.logger.Info("stopping TLS listeners")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var firstErr error
	if m.httpsSrv != nil {
		if err := m.httpsSrv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if m.httpSrv != nil {
		if err := m.httpSrv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	m.httpsSrv = nil
	m.httpSrv = nil
	m.running = false
	return firstErr
}

// Reload diffs the current managed domain set against the DB and calls
// ManageAsync for additions, Unmanage for removals. If the manager is
// not running but the DB now has at least one domain, Reload calls
// Start() - this is the lazy-start path that makes adding the first
// domain from the admin UI live, without a restart.
func (m *Manager) Reload() error {
	desired, err := m.activeDomains()
	if err != nil {
		return fmt.Errorf("listing active project domains: %w", err)
	}

	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		if len(desired) == 0 {
			return nil // still nothing to serve
		}
		return m.Start()
	}

	// Compute diff while holding the lock.
	desiredSet := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		desiredSet[d] = struct{}{}
	}

	var toAdd, toRemove []string
	for d := range desiredSet {
		if _, ok := m.managed[d]; !ok {
			toAdd = append(toAdd, d)
		}
	}
	for d := range m.managed {
		if _, ok := desiredSet[d]; !ok {
			toRemove = append(toRemove, d)
		}
	}
	now := time.Now()
	for _, d := range toAdd {
		m.managed[d] = struct{}{}
		m.since[d] = now
	}
	for _, d := range toRemove {
		delete(m.managed, d)
		delete(m.since, d)
		delete(m.statuses, d)
	}
	m.mu.Unlock()

	if len(toRemove) > 0 {
		m.logger.Info("unmanaging removed domains", "domains", toRemove)
		subjects := make([]certmagic.SubjectIssuer, 0, len(toRemove))
		for _, d := range toRemove {
			// Empty IssuerKey matches any issuer for the subject.
			subjects = append(subjects, certmagic.SubjectIssuer{Subject: d})
		}
		m.cache.RemoveManaged(subjects)
	}
	if len(toAdd) > 0 {
		m.logger.Info("managing new domains", "domains", toAdd)
		if err := m.magic.ManageAsync(context.Background(), toAdd); err != nil {
			m.logger.Error("ManageAsync failed for new domains", "error", err, "domains", toAdd)
		}
		go m.checkHTTPSHealth(20 * time.Second)
	}

	return nil
}

// reconcileInterval is how often the manager re-diffs the DB against the managed
// set on its own. Adding a domain already triggers an immediate Reload through
// the event bus, but a domain whose DNS had not propagated at that moment is
// dropped by filterValidDomains and would otherwise stay unmanaged until someone
// saved the project again. The periodic pass picks it up once DNS resolves, so
// going live never needs a second save. It also retries a Start that failed
// because ports 80/443 were busy at boot.
const reconcileInterval = 5 * time.Minute

// startReconcileLoop launches the periodic Reload. The caller must hold m.mu.
// Idempotent: repeated Start calls reuse the running loop.
func (m *Manager) startReconcileLoop() {
	if m.reconcileStop != nil {
		return
	}
	stop := make(chan struct{})
	m.reconcileStop = stop
	safego.Go("tlsproxy domain reconcile", func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := m.Reload(); err != nil {
					m.logger.Warn("periodic domain reconcile failed", "error", err)
				}
			}
		}
	})
}

// managedSinceFor reports when a domain entered the managed set. The probe
// verdict uses it to tell "ACME has not finished" from "this is broken".
func (m *Manager) managedSinceFor(domain string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.since[domain]
	return t, ok
}

// SubscribeToEvents wires the bus so any project change triggers a Reload.
func (m *Manager) SubscribeToEvents(bus *events.Bus) {
	handler := func(e events.Event) {
		m.logger.Debug("project change detected", "event", e.Type, "project_id", e.ProjectID)
		if err := m.Reload(); err != nil {
			m.logger.Error("Reload failed after project change", "event", e.Type, "project_id", e.ProjectID, "error", err)
		}
	}
	bus.Subscribe(events.EventProjectCreated, handler)
	bus.Subscribe(events.EventProjectUpdated, handler)
	bus.Subscribe(events.EventProjectDeleted, handler)
}

// activeDomains queries the DB outside the manager lock.
func (m *Manager) activeDomains() ([]string, error) {
	return m.activeDomainsLogging(m.logger)
}

// activeDomainsLogging is activeDomains with the logger for domain warnings
// chosen by the caller, so a status poll can ask without re-logging them.
func (m *Manager) activeDomainsLogging(logger *slog.Logger) ([]string, error) {
	projects, err := models.ListPublishedProjects(m.db)
	if err != nil {
		return nil, err
	}
	return filterValidDomains(projects, logger), nil
}

// silentLogger discards everything.
var silentLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// HasActiveDomainsQuiet is HasActiveDomains for callers that run on every request
// (the dashboard's status poll): same answer, but a domain with a configuration
// problem is not logged again each time it is asked.
func (m *Manager) HasActiveDomainsQuiet() bool {
	domains, err := m.activeDomainsLogging(silentLogger)
	return err == nil && len(domains) > 0
}

// HasActiveDomains reports whether at least one published project has a valid public
// domain, i.e. the instance is (or is about to be) reachable on the internet
// rather than only over localhost. Used at startup to decide whether to warn
// about permissive public defaults. A DB error is treated as "no domains" so the
// check never blocks or panics startup.
func (m *Manager) HasActiveDomains() bool {
	domains, err := m.activeDomains()
	if err != nil {
		return false
	}
	return len(domains) > 0
}

// activeDomainsLocked is the same as activeDomains but assumes the caller
// holds m.mu (it doesn't actually use it, but kept separate so future
// callers don't try to re-acquire).
func (m *Manager) activeDomainsLocked() ([]string, error) {
	return m.activeDomains()
}

// filterValidDomains pulls Domain values out of projects, drops anything
// that fails pre-flight validation, and logs warnings/errors. Sorted for
// stable comparison in Reload diffs.
func filterValidDomains(projects []models.Project, logger *slog.Logger) []string {
	var out []string
	for _, p := range projects {
		if p.Domain == nil || *p.Domain == "" {
			continue
		}
		domain := *p.Domain
		check := ValidateDomain(domain)
		if !check.Valid {
			logger.Warn("skipping domain with configuration problem",
				"project_id", p.ID,
				"domain", domain,
				"problem", check.Error,
			)
			continue
		}
		if !check.HTTPSAvailable {
			// Loopback/IP domain: routes fine over plain HTTP on the public
			// port, but no publicly-trusted cert can be issued for it. Keep it
			// out of the ACME-managed set so the issuer doesn't spin on doomed
			// orders. Expected, not a misconfiguration - logged at debug.
			logger.Debug("domain is HTTP-only, not adding to TLS managed set",
				"project_id", p.ID,
				"domain", domain,
			)
			continue
		}
		for _, w := range check.Warnings {
			logger.Warn("domain warning",
				"project_id", p.ID,
				"domain", domain,
				"warning", w,
			)
		}
		out = append(out, domain)
	}
	sort.Strings(out)
	return out
}

// redirectToHTTPS is the fallback for the :80 handler - everything that
// isn't an ACME challenge gets a 301 to the same URL on HTTPS.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
