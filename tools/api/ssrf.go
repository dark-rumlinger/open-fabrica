package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/tools"
)

// SSRF defences for outbound HTTP live in internal/netguard; this file keeps the
// project-local URL helpers (relative project URLs resolve to this instance).

// ssrfSafeClient returns an http.Client hardened against SSRF for external URLs.
// isRelative skips protection entirely for local project URLs.
func ssrfSafeClient(isRelative bool) *http.Client {
	if isRelative {
		// Local URLs skip the private-address check, but a redirect must not carry the
		// call anywhere else: a project endpoint answering 302 to the admin panel or a
		// metadata service would otherwise be fetched with no check at all.
		return &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameHostRedirects}
	}
	return netguard.SSRFSafeClient()
}

// sameHostRedirects follows a redirect only while it stays on the host the call
// started on.
func sameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("redirect to %s leaves the project and was not followed", req.URL.Host)
	}
	return nil
}

// isLocalDomain returns true for localhost, *.localhost, and other local-only domains.
func isLocalDomain(domain string) bool {
	d := strings.ToLower(domain)
	return d == "localhost" || strings.HasSuffix(d, ".localhost")
}

// getProjectDomain returns the project's configured domain, or "" if none.
func getProjectDomain(ctx *tools.ToolContext) string {
	if ctx.GlobalDB != nil && ctx.ProjectID > 0 {
		var domain sql.NullString
		_ = ctx.GlobalDB.QueryRow("SELECT domain FROM projects WHERE id = ?", ctx.ProjectID).Scan(&domain)
		if domain.Valid && domain.String != "" {
			return domain.String
		}
	}
	return ""
}

// resolveLocalURL turns a relative path like "/api/drawings" into a full URL
// using the project's domain or falling back to the local public server.
func resolveLocalURL(ctx *tools.ToolContext, path string) string {
	if domain := getProjectDomain(ctx); domain != "" && !isLocalDomain(domain) {
		return "https://" + domain + path
	}
	return localBaseURL(ctx) + path
}

// localBaseURL is the loopback origin of this instance's public project server,
// honoring the configured public port (OPENFABRICA_PUBLIC_PORT / config.json).
// Falls back to the documented default (5733) when the port is unset so callers
// that construct one without a fully-populated ToolContext still resolve.
func localBaseURL(ctx *tools.ToolContext) string {
	port := ctx.PublicPortOrDefault()
	return fmt.Sprintf("http://localhost:%d", port)
}
