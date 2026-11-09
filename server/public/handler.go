package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/httpx"
	"openfabrica/providers/llm"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

type contextKey string

const projectContextKey contextKey = "resolved_project"

// Handler provides the main public request handlers.
type Handler struct {
	deps     *Deps
	apiCache *apiCache

	// Per-IP throttles for sensitive auth flows. Tuned tighter than the
	// generic dynamic-API rate_limit because each request here either burns
	// a credential-attempt slot, sends an email, or is otherwise expensive
	// to serve under abuse.
	authLimiter         *middleware.RateLimiter // login + register: 5 req/s, burst 10
	magicRequestLimiter *middleware.RateLimiter // /magic/request: 0.5 req/s, burst 3 (each call sends an email)
	magicEmailLimiter   *middleware.RateLimiter // /magic/request per target address: 1 per 20s, burst 3, so a victim's inbox cannot be flooded from many IPs
	magicVerifyLimiter  *middleware.RateLimiter // /magic/verify:  2 req/s, burst 10 (allow legitimate retries, block token grinding)
	totpVerifyLimiter   *middleware.RateLimiter // /totp/verify:    2 req/s, burst 10 (allow legitimate retries, block brute force)

	// authLockout bounds the total guess rate against a single project account
	// (keyed project:endpoint:username) regardless of source IP, so an
	// IP-rotating attacker cannot grind one account past the per-IP authLimiter.
	authLockout *middleware.AccountLockout

	// resolveModel substitutes the project-model resolution in tests; nil uses
	// projectmodel.Resolve.
	resolveModel func(projectID int) (llm.Provider, string, error)
}

// ProjectResolver is middleware that resolves the project from the Host header
// and injects it into the request context.
func (h *Handler) ProjectResolver(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Strip port if present.
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}

		// Each project is reached via its own domain, so resolve purely from the
		// Host header. An unmatched host is a 404 rather than a guess at some
		// other tenant's project.
		project, err := models.GetProjectByDomain(h.deps.DB.DB, host)
		if err != nil {
			if wantsHTML(r) {
				serveErrorPage(w, http.StatusNotFound, "Project Not Found", "")
			} else {
				writeError(w, http.StatusNotFound, "project not found")
			}
			return
		}

		// Block unpublished projects, serve a branded page for browsers, JSON for API clients.
		if !project.Published {
			if wantsHTML(r) {
				serveErrorPage(w, http.StatusNotFound, "Project Unavailable", "This project is currently unavailable. Please check back later.")
			} else {
				writeError(w, http.StatusNotFound, "project not found")
			}
			return
		}

		ctx := context.WithValue(r.Context(), projectContextKey, project)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveCSP returns the Content-Security-Policy for this request. The precedence
// (project override, else instance default) lives in config.ResolveCSP so the
// snapshot the build model reads and the header the browser enforces cannot diverge.
// Used by the CSP middleware (registered after ProjectResolver so the project is in
// context).
func (h *Handler) resolveCSP(r *http.Request) string {
	var override string
	if p := getProject(r); p != nil {
		override = models.ProjectCSP(p)
	}
	return config.ResolveCSP(override, h.deps.PublicCSP)
}

// getProject extracts the resolved project from the context.
func getProject(r *http.Request) *models.Project {
	project, _ := r.Context().Value(projectContextKey).(*models.Project)
	return project
}

// Helpers

// serveDocsJSON serves the auto-generated OpenAPI spec from of_files.
func (h *Handler) serveDocsJSON(w http.ResponseWriter, project *models.Project, projectDB *sql.DB) {
	var storage, body, diskPath sql.NullString
	err := projectDB.QueryRow(
		"SELECT storage, body, disk_path FROM of_files WHERE kind = 'file' AND path = 'api-docs.json' AND scope = 'global' AND is_deleted = 0",
	).Scan(&storage, &body, &diskPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "API docs not generated yet")
		return
	}

	data, err := store.ReadArtifactBytes(storage, body, diskPath, h.deps.ProjectDBManager.ProjectDir(project.ID))
	if err != nil {
		writeError(w, http.StatusNotFound, "API docs not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// writeJSON, writeError and wantsHTML are package-local aliases for the
// canonical helpers in internal/httpx, kept so the many public call sites read
// tersely. The implementation (and the {"error":...} wire shape, shared with
// the admin server) lives in one place now; see internal/httpx.
func writeJSON(w http.ResponseWriter, status int, v any)       { httpx.WriteJSON(w, status, v) }
func writeError(w http.ResponseWriter, status int, msg string) { httpx.WriteError(w, status, msg) }
func wantsHTML(r *http.Request) bool                           { return httpx.WantsHTML(r) }

// openProjectDB resolves a project's DB, writing the standard 500 error and
// returning nil when it cannot be opened - the shape shared by every JSON
// handler. Asset handlers with bespoke fallbacks (a 404 page, the default
// favicon, skip-and-continue loops) open the manager directly instead.
func (h *Handler) openProjectDB(w http.ResponseWriter, projectID int) *db.ProjectDB {
	pdb, err := h.deps.ProjectDBManager.Open(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open project database")
		return nil
	}
	return pdb
}

// basePageCSS is the chrome shared by every standalone public page (the
// error pages and the building placeholder): the reset, the dark gradient
// body, the centered column layout, the muted message line, and the footer.
// Page-specific rules (.code, .scene, the build stepper) live with each
// page. Kept as a string both pages embed so the shared look can't drift.
const basePageCSS = `*{margin:0;padding:0;box-sizing:border-box}
body{min-height:100vh;display:flex;flex-direction:column;justify-content:center;align-items:center;
  background:radial-gradient(ellipse at top,#191e29 0%,#0b0e14 65%);color:#e5e8ef;
  font-family:'Inter',-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Oxygen,Ubuntu,sans-serif;
  text-align:center;padding:24px;line-height:1.6;-webkit-font-smoothing:antialiased}
.message{font-size:.875rem;color:#9aa1b3;max-width:28rem;line-height:1.6}
footer{position:fixed;bottom:0;width:100%;padding:20px;color:#848b9e;font-size:.75rem}
footer a{color:#9aa1b3;text-decoration:none;transition:color .15s}
footer a:hover{color:#e5e8ef}`

// pageFooterHTML is the attribution footer shared by every standalone page.
const pageFooterHTML = `<footer>Powered by <a href="https://github.com/dark-rumlinger/open-fabrica" target="_blank" rel="noopener">Open Fabrica</a></footer>`

// serveErrorPage writes a branded HTML error page styled to match the admin
// dark theme (web/admin/css/tokens.css tokens).
func serveErrorPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>
%s
.code{font-size:5rem;font-weight:700;color:#e5e8ef;line-height:1;letter-spacing:-.02em}
.title{font-size:1.5rem;margin:.75rem 0 .5rem;color:#e5e8ef;font-weight:600;letter-spacing:-.01em}
</style>
</head>
<body>
<div class="code">%d</div>
<div class="title">%s</div>
<div class="message">%s</div>
%s
</body>
</html>`, html.EscapeString(title), basePageCSS, status, html.EscapeString(title), html.EscapeString(message), pageFooterHTML)
}

// serveBuildingPage renders the "factory at work" placeholder shown when a
// project exists but has no published pages yet. stage is the raw pipeline
// stage code, used only to light the matching public step in the progress
// stepper; an empty or unrecognized stage lights nothing rather than guess.
// Returns 503 + Retry-After so crawlers treat it as transient and won't
// index the placeholder. Pure inline SVG + CSS so it has no asset
// dependencies.
func serveBuildingPage(w http.ResponseWriter, stage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "30")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)

	var steps strings.Builder
	for i, s := range publicBuildSteps(stage) {
		if i > 0 {
			steps.WriteString(`<span class="sep"></span>`)
		}
		steps.WriteString(`<span class="step ` + s.state + `"><span class="sd"></span>` + s.label + `</span>`)
	}

	page := strings.NewReplacer(
		"__BASE_CSS__", basePageCSS,
		"__STEPS__", steps.String(),
		"__FOOTER__", pageFooterHTML,
	).Replace(buildingPageTemplate)
	w.Write([]byte(page))
}

// buildStep is one node in the public progress stepper on the building page.
type buildStep struct {
	label string
	state string // "done", "active", or "todo"
}

// publicBuildSteps maps the internal pipeline stage to the three visitor-
// facing steps, marking each done/active/todo. The labels are deliberately
// neutral: this page is shown to anonymous visitors, not the project owner,
// and never carries internal stage codes or build detail. An empty or
// unknown stage leaves every step "todo" so the stepper still renders.
func publicBuildSteps(stage string) []buildStep {
	active := -1 // index of the step the build is on; -1 lights nothing
	// Engine stage codes are lowercase (of_runs.stage); lower-case defensively
	// so a stray legacy uppercase value still maps.
	switch strings.ToLower(stage) {
	case "expand", "plan":
		active = 0
	case "build":
		active = 1
	case "live":
		active = 2
	}
	labels := []string{"Planning", "Building", "Going live"}
	steps := make([]buildStep, len(labels))
	for i, label := range labels {
		state := "todo"
		switch {
		case active < 0:
			state = "todo"
		case i < active:
			state = "done"
		case i == active:
			state = "active"
		}
		steps[i] = buildStep{label: label, state: state}
	}
	return steps
}

// buildingPageTemplate carries placeholder tokens (__BASE_CSS__, __STEPS__,
// __FOOTER__) filled by serveBuildingPage. Tokens rather than fmt verbs so
// the literal % in the CSS keyframes needs no escaping.
const buildingPageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Build in progress...</title>
<style>
__BASE_CSS__
body{overflow:hidden}
.scene{width:min(18rem,70vw);margin-bottom:14px}
.title{font-size:1.5rem;color:#e5e8ef;margin-bottom:8px;font-weight:600;letter-spacing:-.01em}
.steps{display:flex;align-items:center;justify-content:center;gap:10px;margin-top:24px;flex-wrap:wrap}
.step{display:inline-flex;align-items:center;gap:8px;font-size:.8rem;color:#848b9e;font-weight:500;letter-spacing:.01em}
.step .sd{width:8px;height:8px;border-radius:50%;background:#2f3336;flex:none}
.step.done{color:#9aa1b3}
.step.done .sd{background:#848b9e}
.step.active{color:#e5e8ef}
.step.active .sd{background:#e5e8ef;box-shadow:0 0 8px rgba(229,232,239,.5);animation:pulse 1.4s ease-in-out infinite}
.sep{width:16px;height:1px;background:#2f3336;flex:none}
@keyframes pulse{0%,100%{opacity:.35;transform:scale(.85)}50%{opacity:1;transform:scale(1.15)}}
.bp{fill:none;stroke:#848b9e;stroke-width:2;stroke-linecap:round;stroke-linejoin:round;
  stroke-dasharray:600;stroke-dashoffset:600;animation:draw 5s ease-in-out infinite}
.bp.l2{animation-delay:.4s}
.bp.l3{animation-delay:.8s}
.bp.l4{animation-delay:1.1s}
.bp.l5{animation-delay:1.4s}
.bp.l6{animation-delay:1.7s}
@keyframes draw{0%{stroke-dashoffset:600;stroke:#848b9e}35%{stroke-dashoffset:0;stroke:#e5e8ef}75%{stroke-dashoffset:0;stroke:#848b9e;opacity:1}92%,100%{stroke-dashoffset:0;opacity:0}}
@media (prefers-reduced-motion: reduce){
  .bp{animation:none;stroke-dashoffset:0;stroke:#848b9e;opacity:1}
  .step.active .sd{animation:none;opacity:1;transform:none}
}
</style>
</head>
<body>
<svg class="scene" viewBox="32 24 136 122" xmlns="http://www.w3.org/2000/svg">
  <rect class="bp"    x="45" y="35" width="110" height="100" rx="4"/>
  <path class="bp l2" d="M45 58 H155"/>
  <circle class="bp l3" cx="56" cy="46" r="3"/>
  <path class="bp l4" d="M62 80 H100 M62 95 H104 M62 110 H92"/>
  <rect class="bp l5" x="112" y="74" width="34" height="46" rx="3"/>
  <path class="bp l6" d="M120 92 H138 M120 104 H138"/>
</svg>
<div class="title">Open Fabrica is building here</div>
<div class="message">Come back in a moment.</div>
<div class="steps">__STEPS__</div>
__FOOTER__
</body>
</html>`

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// projectKeyOf names the request's project for rate-limit keys, so two projects
// that happen to share an endpoint path do not share a visitor's quota.
func projectKeyOf(r *http.Request) string {
	if p := getProject(r); p != nil {
		return strconv.Itoa(p.ID)
	}
	return "0"
}
