package project

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/dbh"
	"openfabrica/internal/favicon"
	"openfabrica/internal/projectphase"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_project covers the project-level ops: read the project record and
// scaffold PWA support. It also exports redirect helpers
// (ProjectAddRedirect/Remove/List) used by other tools. It deliberately cannot
// change the project's phase: only the owner and the engine move a project
// between building, live and paused, because a write from here would reach
// neither the running worker nor the admin UI.

// RequirePagesFirst wraps an action handler so calls return a clear
// "headless project" error when no page artifacts exist. Exported for reuse
// by other tools (SEO, redirects) that need the same guard.
func RequirePagesFirst(action string, h tools.ActionHandler) tools.ActionHandler {
	return func(ctx *tools.ToolContext, args map[string]any) (*tools.Result, error) {
		if r := requirePages(ctx, action); r != nil {
			return r, nil
		}
		return h(ctx, args)
	}
}

func requirePages(ctx *tools.ToolContext, action string) *tools.Result {
	var n int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0").Scan(&n)
	if n > 0 {
		return nil
	}
	return tools.Fail("action %q needs at least one saved page artifact first; none exist yet (the plan may declare pages, but this check counts saved pages, not planned ones). Save a page, then retry.", action)
}

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_project",
		Category: "project",
		OneLiner: "Read project info (phase, published) and enable PWA.",
		WhenToUse: `Use info to read the project record (name, phase, whether it is published,
timestamps). Use enable_pwa LAST, once every page is saved (the service
worker precaches only routes that exist when it runs) and the owner wants it installable. enable_pwa produces a complete, working manifest,
service worker, and offline page - do not overwrite service-worker.js or
manifest.json afterwards; they are final.`,
		Actions: []tools.Action{
			&ProjectInfo{},
			&ProjectEnablePWA{},
		},
	})
}

// ProjectInfo returns the project's record from the global DB.
type ProjectInfo struct{}

func (a *ProjectInfo) Doc() string    { return "Return project record from the global DB." }
func (a *ProjectInfo) ReadOnly() bool { return true }
func (a *ProjectInfo) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var name, mode string
	var published bool
	var domain, description sql.NullString
	var createdAt, updatedAt time.Time
	if err := ctx.GlobalDB.QueryRow(
		"SELECT name, domain, mode, published, description, created_at, updated_at FROM projects WHERE id = ?",
		ctx.ProjectID,
	).Scan(&name, &domain, &mode, &published, &description, &createdAt, &updatedAt); err != nil {
		return tools.NotFound("project", ""), nil
	}
	// A tool runs inside a live worker, so the engine is running.
	phase := projectphase.LoadFor(ctx.DB, mode, true)
	result := map[string]any{
		"name":       name,
		"phase":      map[string]any{"key": phase.Key, "label": phase.Label, "reason": phase.Reason},
		"published":  published,
		"created_at": createdAt, "updated_at": updatedAt,
	}
	if domain.Valid && domain.String != "" {
		result["domain"] = domain.String
	}
	if description.Valid && description.String != "" {
		result["description"] = description.String
	}
	return tools.Ok(result), nil
}

// ProjectEnablePWA scaffolds Progressive Web App support: manifest.json
// (with design-system colors), service-worker.js with cache-first/network-
// fallback strategy, an offline page, and the matching <link>/<script>
// hooks patched into the default layout.
type ProjectEnablePWA struct{}

func (a *ProjectEnablePWA) ActionName() string { return "enable_pwa" }
func (a *ProjectEnablePWA) Doc() string {
	return "Scaffold a COMPLETE Progressive Web App: manifest, a resilient service worker (precaches page routes, lazily caches /files/*), offline page, and layout patch. The generated service worker and manifest are final - do not overwrite them."
}

// Run is gated by RequirePagesFirst via the manage_project wrapper below.
func (a *ProjectEnablePWA) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if r := requirePages(ctx, "enable_pwa"); r != nil {
		return r, nil
	}
	var projectName string
	if err := ctx.GlobalDB.QueryRow(
		"SELECT name FROM projects WHERE id = ?", ctx.ProjectID,
	).Scan(&projectName); err != nil {
		return tools.Fail("could not read project name"), nil
	}

	themeColor, bgColor := "#2563eb", "#ffffff"
	planPrimary := ""
	var planJSON sql.NullString
	_ = ctx.DB.QueryRow(`SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON)
	if planJSON.Valid && planJSON.String != "" {
		var plan struct {
			DesignSystem struct {
				Colors struct {
					Primary string `json:"primary"`
					Bg      string `json:"bg"`
				} `json:"colors"`
			} `json:"design_system"`
		}
		if json.Unmarshal([]byte(planJSON.String), &plan) == nil {
			// Written through verbatim, so only a literal a browser accepts may
			// pass; a planner annotation ("#d8b4b8 (rose quartz)") keeps the default
			// rather than shipping a manifest colour no browser can parse.
			// Kept raw for the favicon, which accepts a bare #rgb/#rrggbb and falls back
			// on anything else; the manifest needs a literal a browser parses.
			planPrimary = plan.DesignSystem.Colors.Primary
			if c := manifestColor(plan.DesignSystem.Colors.Primary); c != "" {
				themeColor = c
			}
			if c := manifestColor(plan.DesignSystem.Colors.Bg); c != "" {
				bgColor = c
			}
		}
	}

	// A PWA is only installable with at least one manifest icon. The project's
	// favicon.svg doubles as the app icon, but the platform's own favicon
	// generator (engine.ensureFavicon) runs at build FINALISATION - after the
	// build agent calls enable_pwa - so without this the manifest would freeze
	// with an empty icons array and no browser would offer to install the app.
	// Generate the same deterministic favicon now if none exists yet (a model- or
	// prior-build-authored favicon.svg is left untouched); the finaliser then sees
	// it and skips, so the mark stays identical wherever it was created.
	if !store.ArtifactExists(ctx.DB, "file", "favicon.svg") {
		if _, _, err := store.ArtifactSave(ctx.DB, &store.Artifact{
			Kind:        "file",
			Path:        "favicon.svg",
			Scope:       "global",
			ContentType: "image/svg+xml",
			Body:        favicon.SVGWithPrimary(projectName, planPrimary),
		}, ctx.ChangedBy()); err != nil {
			return nil, fmt.Errorf("generating PWA app icon (favicon.svg): %w", err)
		}
	}

	icons := collectManifestIcons(ctx.DB)

	manifest := map[string]any{
		"name": projectName, "short_name": projectName,
		"start_url": "/", "display": "standalone",
		"theme_color": themeColor, "background_color": bgColor,
		"icons": icons,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling manifest: %w", err)
	}
	if err := savePWAAsset(ctx, "manifest.json", "application/manifest+json", manifestBytes); err != nil {
		return nil, fmt.Errorf("saving manifest.json: %w", err)
	}

	// Build a resilient precache list from real, resolvable URLs: the offline
	// page, the manifest, and every static page route. Dynamic param routes
	// (containing ':') are skipped because they don't resolve as-is. CSS/JS
	// are cached lazily by the /files/ runtime handler below, so they're
	// intentionally left out of the precache. This keeps the list to URLs
	// that actually return 2xx, which matters because the install caches them
	// individually (cache.add per URL) and never via the atomic cache.addAll.
	precache := []string{"/offline", "/manifest.json"}
	seen := map[string]bool{"/offline": true, "/manifest.json": true}
	if pageRows, _ := ctx.DB.Query("SELECT path FROM of_files WHERE kind = 'page' AND is_deleted = 0"); pageRows != nil {
		for pageRows.Next() {
			var p string
			if pageRows.Scan(&p) != nil || strings.Contains(p, ":") || seen[p] {
				continue
			}
			seen[p] = true
			precache = append(precache, p)
		}
		_ = pageRows.Err() // best-effort precache list; a partial list still works
		pageRows.Close()
	}
	precacheJSON, _ := json.Marshal(precache)

	swJS := `const CACHE_NAME = 'openfabrica-pwa-v1';
const OFFLINE_URL = '/offline';
const PRECACHE = ` + string(precacheJSON) + `;

self.addEventListener('install', (event) => {
  // Cache each URL individually so one bad/404/redirected entry can't abort
  // the whole install the way cache.addAll() would.
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => Promise.allSettled(PRECACHE.map((url) => cache.add(url))))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', (event) => {
  if (event.request.method !== 'GET') return;

  const url = new URL(event.request.url);

  // Static assets: cache-first, populating the cache on first use.
  if (url.pathname.startsWith('/files/')) {
    event.respondWith(
      caches.match(event.request).then((cached) => cached || fetch(event.request).then((response) => {
        const clone = response.clone();
        caches.open(CACHE_NAME).then((cache) => cache.put(event.request, clone));
        return response;
      }))
    );
    return;
  }

  // Page navigations: network-first, and cache each clean 2xx response so a
  // route visited online is available offline next time - including routes
  // built AFTER enable_pwa ran, which the static PRECACHE list never captured.
  // On a network failure, fall back to the cached page, then the offline page.
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetch(event.request)
        .then((response) => {
          // Only cache a clean, non-redirected 2xx page; never an error or a
          // redirect, which would poison the offline cache.
          if (response.ok && !response.redirected) {
            const clone = response.clone();
            caches.open(CACHE_NAME).then((cache) => cache.put(event.request, clone));
          }
          return response;
        })
        .catch(() =>
          caches.match(event.request).then((cached) => cached || caches.match(OFFLINE_URL))
        )
    );
    return;
  }
});

// Web Push: manage_push sends {title, body, url}. Show it, and open the target
// page when the notification is clicked.
self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (e) { data = { body: event.data ? event.data.text() : '' }; }
  const title = data.title || self.registration.scope;
  event.waitUntil(self.registration.showNotification(title, {
    body: data.body || '',
    icon: data.icon || undefined,
    tag: data.tag || undefined,
    data: { url: data.url || '/' },
  }));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const target = new URL((event.notification.data && event.notification.data.url) || '/', self.location.origin).href;
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((wins) => {
      for (const w of wins) {
        if (w.url === target && 'focus' in w) return w.focus();
      }
      return self.clients.openWindow(target);
    })
  );
});`
	if err := savePWAAsset(ctx, "service-worker.js", "application/javascript", []byte(swJS)); err != nil {
		return nil, fmt.Errorf("saving service-worker.js: %w", err)
	}

	offlineHTML := `<section style="display:flex;align-items:center;justify-content:center;min-height:80vh;text-align:center;padding:2rem;">
  <div>
    <h1>You're offline</h1>
    <p>Please check your internet connection and try again.</p>
  </div>
</section>`
	if _, _, err := store.ArtifactSave(ctx.DB, &store.Artifact{
		Kind:        "page",
		Path:        "/offline",
		Body:        offlineHTML,
		ContentType: "text/html; charset=utf-8",
		Metadata:    map[string]any{"title": "Offline"},
	}, ctx.ChangedBy()); err != nil {
		return nil, fmt.Errorf("saving offline page: %w", err)
	}

	var template sql.NullString
	_ = ctx.DB.QueryRow("SELECT body FROM of_files WHERE kind = 'layout' AND path = 'default' AND is_deleted = 0").Scan(&template)
	pwaSnippet := "\n  <link rel=\"manifest\" href=\"/manifest.json\">\n  <script>if('serviceWorker' in navigator){navigator.serviceWorker.register('/service-worker.js')}</script>\n"
	layoutState := "patched"
	switch {
	case !template.Valid:
		layoutState = "no_default_layout"
	case strings.Contains(template.String, "/manifest.json"):
		layoutState = "already_linked"
	default:
		lower := strings.ToLower(template.String)
		idx := strings.Index(lower, "</head>")
		if idx < 0 {
			layoutState = "no_head_tag"
			break
		}
		patched := template.String[:idx] + pwaSnippet + template.String[idx:]
		if _, err := ctx.DB.Exec(
			"UPDATE of_files SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'layout' AND path = 'default'",
			patched,
		); err != nil {
			return nil, fmt.Errorf("patching layout template: %w", err)
		}
	}
	out := map[string]any{
		"files":  []string{"manifest.json", "service-worker.js", "/offline"},
		"layout": layoutState,
		"message": "PWA enabled. The service worker precaches the routes that exist right now and " +
			"caches other pages on first visit, so run enable_pwa last (after every page is saved) for " +
			"full first-load offline coverage. Edit /offline's copy/styling if you want.",
	}
	// Without the manifest link and the registration the browser never installs the
	// app, so a skipped patch is not a success to report quietly.
	if layoutState == "no_default_layout" || layoutState == "no_head_tag" {
		out["warning"] = "the manifest link and service-worker registration were NOT added: there is no default layout with a </head> to put them in. Save the shared layout at path \"default\" (with {{assets}} in <head>), then run enable_pwa again."
	}
	return tools.Ok(out), nil
}

// collectManifestIcons builds the manifest `icons` array from the icon files a
// project actually has. enable_pwa guarantees favicon.svg exists before calling
// this, so the array is never empty - an empty `icons` array makes a PWA
// non-installable (no browser offers to add it to the home screen), which is the
// symptom this prevents. A single SVG with sizes "any" satisfies every
// required icon size, so favicon.svg alone is enough for the install prompt;
// favicon.png / apple-touch-icon.png are added when the build authored them.
func collectManifestIcons(db dbh.Handle) []map[string]string {
	icons := []map[string]string{}
	rows, _ := db.Query(
		`SELECT path FROM of_files
		 WHERE kind = 'file' AND is_deleted = 0
		   AND path IN ('favicon.svg', 'favicon.png', 'apple-touch-icon.png')`,
	)
	if rows == nil {
		return icons
	}
	defer rows.Close()
	for rows.Next() {
		var fn string
		if rows.Scan(&fn) != nil {
			continue
		}
		switch fn {
		case "favicon.svg":
			icons = append(icons, map[string]string{"src": "/files/favicon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any maskable"})
		case "favicon.png":
			icons = append(icons, map[string]string{"src": "/files/favicon.png", "sizes": "512x512", "type": "image/png"})
		case "apple-touch-icon.png":
			icons = append(icons, map[string]string{"src": "/files/apple-touch-icon.png", "sizes": "180x180", "type": "image/png"})
		}
	}
	_ = rows.Err() // best-effort; favicon.svg is guaranteed before the loop
	return icons
}

// savePWAAsset writes a PWA file to disk and records it in of_files as
// a global-scope artifact.
func savePWAAsset(ctx *tools.ToolContext, filename, contentType string, data []byte) error {
	storagePath, err := store.WriteFileToDisk(ctx.DataDirOrDefault(), ctx.ProjectID, "assets", filename, data)
	if err != nil {
		return err
	}
	_, err = ctx.DB.Exec(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, scope, metadata, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, 'global', '{}', 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   scope = excluded.scope,
		   updated_at = CURRENT_TIMESTAMP`,
		filename, contentType, len(data), storagePath,
	)
	return err
}

// Redirect helpers exported for use by the content/manage_redirects tool.

func ProjectAddRedirect(ctx *tools.ToolContext, source, target string, statusCode int) (*tools.Result, error) {
	if source == "" || target == "" {
		return tools.Fail("redirect_from and redirect_to are required"), nil
	}
	if source[0] != '/' {
		return tools.Fail("redirect_from must start with /"), nil
	}
	if statusCode != 301 && statusCode != 302 {
		return tools.Fail("status_code must be 301 (permanent) or 302 (temporary)"), nil
	}
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_redirects (redirect_from, redirect_to, status_code) VALUES (?, ?, ?)
		 ON CONFLICT(redirect_from) DO UPDATE SET redirect_to = excluded.redirect_to, status_code = excluded.status_code`,
		source, target, statusCode,
	); err != nil {
		return nil, fmt.Errorf("adding redirect: %w", err)
	}
	return tools.Ok(map[string]any{
		"redirect_from": source, "redirect_to": target, "status_code": statusCode,
	}), nil
}

func ProjectRemoveRedirect(ctx *tools.ToolContext, source string) (*tools.Result, error) {
	if source == "" {
		return tools.Fail("redirect_from is required"), nil
	}
	res, err := ctx.DB.Exec("DELETE FROM of_redirects WHERE redirect_from = ?", source)
	if err != nil {
		return nil, fmt.Errorf("removing redirect: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.NotFound("redirect", source), nil
	}
	return tools.Ok(map[string]any{"deleted": source}), nil
}

func ProjectListRedirects(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT redirect_from, redirect_to, status_code, created_at FROM of_redirects ORDER BY redirect_from")
	if err != nil {
		return nil, fmt.Errorf("listing redirects: %w", err)
	}
	defer rows.Close()
	var redirects []map[string]any
	for rows.Next() {
		var source, target string
		var statusCode int
		var createdAt time.Time
		if err := rows.Scan(&source, &target, &statusCode, &createdAt); err != nil {
			continue
		}
		redirects = append(redirects, map[string]any{
			"redirect_from": source, "redirect_to": target,
			"status_code": statusCode, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing redirects: %w", err)
	}
	return tools.Ok(redirects), nil
}

// manifestColor returns v if it is a bare CSS hex colour (#rgb, #rgba, #rrggbb,
// #rrggbbaa), else "" so the caller keeps its default.
func manifestColor(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 4 || v[0] != '#' {
		return ""
	}
	for i := 1; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	switch len(v) - 1 {
	case 3, 4, 6, 8:
		return v
	}
	return ""
}
