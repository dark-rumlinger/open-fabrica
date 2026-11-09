package admin

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"openfabrica/internal/netinfo"
	"openfabrica/security"
	"openfabrica/server/deps"
	"openfabrica/server/middleware"
)

// Deps holds all admin handler dependencies: the server-shared deps.Core,
// with Logger overwritten to the admin-labeled child by server.New.
type Deps struct {
	deps.Core
}

// adminHandlers bundles every admin API handler. Bundling them keeps the
// route-group registration helpers to two parameters (router + handlers)
// instead of threading thirty handlers through each one.
type adminHandlers struct {
	auth              *AuthHandler
	preferences       *PreferencesHandler
	projects          *ProjectsHandler
	engine            *EngineHandler
	chat              *ChatHandlerAdmin
	providers         *ProvidersHandler
	svcProviders      *ServiceProvidersHandler
	users             *UsersHandler
	guides            *GuidesHandler
	projectMemory     *ProjectMemoryHandler
	system            *SystemHandler
	questions         *QuestionsHandler
	settings          *SettingsHandler
	projectPages      *ProjectPagesHandler
	projectTables     *ProjectTablesHandler
	projectTasks      *ProjectTasksHandler
	projectEndpoints  *ProjectEndpointsHandler
	projectFiles      *ProjectFilesHandler
	projectWebhooks   *ProjectWebhooksHandler
	projectTriggers   *ProjectTriggersHandler
	projectJobs       *ProjectJobsHandler
	projectSpend      *ProjectSpendHandler
	projectAuthEnds   *ProjectAuthEndpointsHandler
	projectSecrets    *ProjectSecretsHandler
	projectPayments   *ProjectPaymentsHandler
	projectTransfer   *ProjectTransferHandler
	projectLayouts    *ProjectLayoutsHandler
	projectComponents *ProjectComponentsHandler
	projectAnalytics  *ProjectAnalyticsHandler
	projectDiag       *ProjectDiagnosticsHandler
	builds            *BuildsHandler
	bot               *BotHandler
	firewall          *FirewallHandler
	triage            *TriageHandler
	hosting           *HostingHandler
	audit             *AuditHandler
	backups           *BackupsHandler
}

// newAdminHandlers constructs every admin handler from the shared deps.
func newAdminHandlers(deps *Deps) *adminHandlers {
	h := &adminHandlers{
		auth: &AuthHandler{
			deps: deps,
			// Per-username lockout (see middleware tunables): AdminLockoutAttempts
			// wrong passwords lock the account, then exponential backoff from
			// LockoutBaseDelay to LockoutMaxDelay. Per-username, so it complements
			// the per-IP authLimiter without locking out unrelated accounts.
			lockout: middleware.NewAccountLockout(middleware.AdminLockoutAttempts, middleware.LockoutBaseDelay, middleware.LockoutMaxDelay),
		},
		preferences:       &PreferencesHandler{deps: deps},
		projects:          &ProjectsHandler{deps: deps},
		engine:            &EngineHandler{deps: deps},
		chat:              &ChatHandlerAdmin{deps: deps},
		providers:         &ProvidersHandler{deps: deps},
		svcProviders:      &ServiceProvidersHandler{deps: deps},
		users:             &UsersHandler{deps: deps},
		guides:            &GuidesHandler{deps: deps},
		projectMemory:     &ProjectMemoryHandler{deps: deps},
		system:            &SystemHandler{deps: deps},
		questions:         &QuestionsHandler{deps: deps},
		settings:          &SettingsHandler{deps: deps},
		projectPages:      &ProjectPagesHandler{deps: deps},
		projectTables:     &ProjectTablesHandler{deps: deps},
		projectTasks:      &ProjectTasksHandler{deps: deps},
		projectEndpoints:  &ProjectEndpointsHandler{deps: deps},
		projectFiles:      &ProjectFilesHandler{deps: deps},
		projectWebhooks:   &ProjectWebhooksHandler{deps: deps},
		projectTriggers:   &ProjectTriggersHandler{deps: deps},
		projectJobs:       &ProjectJobsHandler{deps: deps},
		projectSpend:      &ProjectSpendHandler{deps: deps},
		projectAuthEnds:   &ProjectAuthEndpointsHandler{deps: deps},
		projectSecrets:    &ProjectSecretsHandler{deps: deps},
		projectPayments:   &ProjectPaymentsHandler{deps: deps},
		projectTransfer:   &ProjectTransferHandler{deps: deps},
		projectLayouts:    &ProjectLayoutsHandler{deps: deps},
		projectComponents: &ProjectComponentsHandler{deps: deps},
		projectAnalytics:  &ProjectAnalyticsHandler{deps: deps},
		projectDiag:       &ProjectDiagnosticsHandler{deps: deps},
		builds:            &BuildsHandler{deps: deps},
		bot:               &BotHandler{deps: deps},
		firewall:          &FirewallHandler{deps: deps},
		triage:            &TriageHandler{deps: deps},
		hosting:           &HostingHandler{deps: deps, net: networkDetector(deps)},
		audit:             &AuditHandler{deps: deps},
		backups:           &BackupsHandler{deps: deps},
	}
	h.auth.armSetupToken()
	return h
}

// RegisterRoutes mounts all admin API routes on the given router. The route tree
// is: public endpoints, then a JWT-protected group (shared read-only routes, the
// per-project subtree, and an admin-only group), then the SPA fallback.
func RegisterRoutes(r chi.Router, deps *Deps, adminFS fs.FS) {
	h := newAdminHandlers(deps)
	authLimiter := middleware.NewRateLimiter(middleware.AdminLoginRatePerSec, middleware.AdminLoginBurst)

	// When the admin surface is exposed off the loopback interface (typically
	// behind a reverse proxy), RemoteAddr may be the proxy's own 127.0.0.1 and
	// can no longer prove the caller is genuinely local. The unauthenticated,
	// loopback-only triage route is therefore only safe while admin binds to
	// loopback; when exposed it moves into the JWT-protected group below.
	adminExposed := deps.Config.AdminExposed()

	r.Route("/admin/api", func(r chi.Router) {
		registerPublicRoutes(r, h, authLimiter, adminExposed)

		// SSE stream: EventSource cannot set an Authorization header, so this one
		// route also accepts the JWT from a ?token= query param. Isolated in its
		// own group so the query-string-token path (which leaks into logs and
		// referrers) is never exposed on any non-SSE admin route.
		r.Group(func(r chi.Router) {
			r.Use(deps.JWTManager.AuthenticatorAllowingQueryToken)
			r.Use(adminAuthGuard(deps))
			r.Get("/events/stream", h.system.EventStream)
		})

		// Protected endpoints (JWT required).
		r.Group(func(r chi.Router) {
			r.Use(deps.JWTManager.Authenticator)
			// Reject project-scoped tokens and tokens invalidated by a password
			// change / sign-out-everywhere before any admin handler runs.
			r.Use(adminAuthGuard(deps))

			// Cross-project build-anomaly triage. Loopback-unauthenticated by
			// default (see registerPublicRoutes); when admin is exposed the
			// loopback check is spoofable, so it is JWT-gated here instead.
			if adminExposed {
				r.Get("/triage", h.triage.Scan)
			}

			// Every signed-in user manages their own credentials and sessions.
			r.Put("/auth/password", h.auth.HandleChangePassword)
			r.Get("/auth/preferences", h.preferences.Get)
			r.Put("/auth/preferences", h.preferences.Update)
			r.Post("/auth/revoke-sessions", h.auth.HandleRevokeSessions)

			registerSharedReadRoutes(r, h)
			r.Route("/projects/{projectID}", func(r chi.Router) {
				registerProjectScopedRoutes(r, h)
			})
			registerEngineAndChatRoutes(r, h)
			r.Group(func(r chi.Router) {
				r.Use(security.RequireRole("admin"))
				registerAdminOnlyRoutes(r, h)
			})
		})
	})

	registerAdminSPA(r, adminFS, deps.Version)
}

// registerPublicRoutes mounts the unauthenticated setup/login endpoints. Login
// and setup are wrapped in the per-IP rate limiter.
func registerPublicRoutes(r chi.Router, h *adminHandlers, authLimiter *middleware.RateLimiter, adminExposed bool) {
	r.Get("/setup/check", h.auth.HandleSetupCheck)
	r.Get("/setup/providers", h.auth.HandleProviderCatalog)
	r.Post("/setup/providers/{providerID}/sync", h.auth.HandleSetupSyncProvider)
	// Cross-project build-anomaly triage. Not JWT-gated: it is mounted only for
	// loopback peers (same 127.0.0.1 trust boundary the whole admin panel binds
	// to), so the local operator can scan build history without a login round-trip.
	// Read-only. Only safe while admin binds to loopback: when the admin surface
	// is exposed, RemoteAddr can be a proxy's 127.0.0.1, so this route is omitted
	// here and JWT-gated in the protected group instead (see RegisterRoutes).
	if !adminExposed {
		r.With(requireLoopback).Get("/triage", h.triage.Scan)
	}
	r.Group(func(r chi.Router) {
		r.Use(authLimiter.Handler)
		r.Post("/auth/login", h.auth.HandleLogin)
		r.Post("/auth/logout", h.auth.HandleLogout)
		r.Post("/setup", h.auth.HandleSetup)
		r.Post("/setup/token", h.auth.HandleSetupTokenCheck)
	})
}

// requireLoopback rejects any request whose peer is not on the loopback
// interface. It fences endpoints that are intentionally unauthenticated but must
// never be reachable off-box (the triage scan) to the same 127.0.0.1 boundary the
// admin listener already binds to.
func requireLoopback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A loopback peer that carries forwarding headers is a same-host reverse
		// proxy relaying someone else, not the local operator.
		if !middleware.IsLoopbackAddr(r.RemoteAddr) || middleware.HasProxyHeaders(r) {
			writeError(w, http.StatusForbidden, "loopback-only endpoint")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerSharedReadRoutes mounts the read-only endpoints available to every
// authenticated user (including the unified Builds view).
func registerSharedReadRoutes(r chi.Router, h *adminHandlers) {
	r.Get("/projects", h.projects.List)
	r.Get("/providers/catalog", h.providers.Catalog)
	r.Get("/providers/transports", h.providers.Transports)
	r.Get("/providers", h.providers.List)
	r.Get("/providers/{providerID}", h.providers.Get)
	r.Get("/providers/{providerID}/models", h.providers.Models)
	r.Get("/users", h.users.List)
	r.Get("/guides", h.guides.List)
	r.Get("/system/status", h.system.Status)
	r.Get("/domain/validate", h.projects.ValidateDomain)
	r.Get("/firewall/status", h.firewall.Status)
	// /events/stream (SSE) is mounted separately in RegisterRoutes so it can
	// accept a ?token= query param that the other routes here must not.
	r.Get("/events/backfill", h.system.EventsBackfill)
	r.Get("/questions", h.questions.List)

	// Unified Builds view: per-project build registry,
	// per-build chronological timeline and per-LLM-call detail blob.
	r.Get("/projects/{projectID}/builds", h.builds.List)
	r.Get("/projects/{projectID}/builds/{buildID}/timeline", h.builds.Timeline)
	r.Get("/projects/{projectID}/builds/{buildID}/export", h.builds.Export)
	r.Get("/projects/{projectID}/builds/{buildID}/cost", h.builds.Cost)
	r.Get("/projects/{projectID}/builds/llm/{logID}", h.builds.LLMCallDetail)
	r.Get("/projects/{projectID}/llm/stats", h.builds.Stats)
}

// registerProjectScopedRoutes mounts everything under /projects/{projectID}: the
// read-only views for any authenticated user, then an admin-only group of the
// destructive project actions.
func registerProjectScopedRoutes(r chi.Router, h *adminHandlers) {
	// Read-only, all authenticated users
	r.Get("/", h.projects.Get)
	r.Get("/summary", h.projects.Summary)
	r.Get("/pages", h.projectPages.List)
	r.Get("/pages/{pageID}", h.projectPages.Get)
	r.Get("/tables", h.projectTables.List)
	r.Get("/tables/{tableName}/rows", h.projectTables.Rows)
	r.Get("/tables/{tableName}/csv", h.projectTables.ExportCSV)
	r.Get("/endpoints", h.projectEndpoints.List)
	r.Get("/endpoints/{endpointID}", h.projectEndpoints.Get)
	r.Get("/files", h.projectFiles.List)
	r.Get("/files/{fileID}/content", h.projectFiles.Content)
	r.Get("/files/{fileID}/versions", h.projectFiles.ListVersions)
	r.Get("/tasks", h.projectTasks.List)
	r.Get("/build-queue", h.projectTasks.BuildQueue)
	r.Get("/webhooks", h.projectWebhooks.List)
	r.Get("/webhooks/{webhookID}", h.projectWebhooks.Get)
	r.Get("/triggers", h.projectTriggers.List)
	r.Get("/jobs", h.projectJobs.List)
	r.Get("/spend-limits", h.projectSpend.Get)
	r.Get("/tasks/{taskID}/runs", h.projectTasks.TaskRuns)
	r.Get("/questions", h.questions.ListByProject)
	r.Get("/service-providers", h.svcProviders.List)
	r.Get("/auth-endpoints", h.projectAuthEnds.List)
	r.Get("/secrets", h.projectSecrets.List)
	r.Get("/memory", h.projectMemory.List)

	r.Get("/layouts", h.projectLayouts.List)
	r.Get("/layouts/{layoutID}", h.projectLayouts.Get)
	r.Get("/components", h.projectComponents.List)
	r.Get("/components/{componentID}", h.projectComponents.Get)
	r.Get("/pages/{pageID}/versions", h.projectPages.ListVersions)
	r.Get("/layouts/{layoutID}/versions", h.projectLayouts.ListVersions)
	r.Get("/components/{componentID}/versions", h.projectComponents.ListVersions)
	r.Get("/analytics/summary", h.projectAnalytics.Summary)
	r.Get("/diagnostics/health", h.projectDiag.Health)
	r.Get("/diagnostics/integrity", h.projectDiag.Integrity)
	r.Get("/diagnostics/errors", h.projectDiag.Errors)
	r.Get("/hosting/status", h.hosting.Status)

	// Admin-only, destructive project actions
	r.Group(func(r chi.Router) {
		r.Use(security.RequireRole("admin"))
		// Record every mutation here (project update/delete, table and artifact
		// writes, secrets, ...) to the admin audit trail.
		r.Use(h.audit.record)
		// Payments and subscriptions: buyer references and amounts, owner-only.
		r.Get("/payments", h.projectPayments.Overview)
		// Export bundles the full project (incl. all dynamic table
		// data), so it is gated like import and the destructive ops.
		r.Get("/export", h.projectTransfer.Export)
		r.Post("/hosting/recheck", h.hosting.Recheck)
		r.Get("/hosting/qr", h.hosting.QR)
		r.Put("/", h.projects.Update)
		r.Delete("/", h.projects.Delete)
		r.Put("/published", h.projects.SetPublished)
		r.Put("/spend-limits", h.projectSpend.Update)
		r.Post("/tables/{tableName}/rows", h.projectTables.InsertRow)
		r.Post("/tables/{tableName}/columns", h.projectTables.AddColumn)
		r.Put("/tables/{tableName}/rows/{rowID}", h.projectTables.UpdateRow)
		r.Delete("/tables/{tableName}/rows/{rowID}", h.projectTables.DeleteRow)
		r.Post("/endpoints", h.projectEndpoints.Create)
		r.Put("/endpoints/{endpointID}", h.projectEndpoints.Update)
		r.Delete("/endpoints/{endpointID}", h.projectEndpoints.Delete)
		r.Post("/files", h.projectFiles.Create)
		r.Put("/files/{fileID}/content", h.projectFiles.UpdateContent)
		r.Post("/files/{fileID}/restore", h.projectFiles.Restore)
		r.Delete("/files/{fileID}", h.projectFiles.Delete)
		r.Post("/tasks/{taskID}/toggle", h.projectTasks.Toggle)
		r.Post("/tasks/{taskID}/run", h.projectTasks.RunNow)
		r.Delete("/tasks/{taskID}", h.projectTasks.Delete)
		r.Post("/tasks", h.projectTasks.Create)
		r.Put("/tasks/{taskID}", h.projectTasks.Update)
		r.Post("/webhooks", h.projectWebhooks.Create)
		r.Put("/webhooks/{webhookID}", h.projectWebhooks.Update)
		r.Post("/webhooks/{webhookID}/toggle", h.projectWebhooks.Toggle)
		r.Post("/webhooks/{webhookID}/test", h.projectWebhooks.Test)
		r.Delete("/webhooks/{webhookID}", h.projectWebhooks.Delete)
		r.Post("/triggers/{triggerID}/toggle", h.projectTriggers.Toggle)
		r.Delete("/triggers/{triggerID}", h.projectTriggers.Delete)
		r.Post("/jobs/{jobID}/cancel", h.projectJobs.Cancel)
		r.Post("/pages", h.projectPages.Create)
		r.Put("/pages/{pageID}", h.projectPages.Update)
		r.Post("/pages/{pageID}/restore", h.projectPages.Restore)
		r.Delete("/pages/{pageID}", h.projectPages.Delete)
		r.Delete("/auth-endpoints/{endpointID}", h.projectAuthEnds.Delete)
		r.Post("/secrets", h.projectSecrets.Create)
		r.Put("/secrets/{secretID}", h.projectSecrets.Update)
		r.Delete("/secrets/{secretID}", h.projectSecrets.Delete)
		r.Post("/memory", h.projectMemory.Create)
		r.Put("/memory/{memoryID}", h.projectMemory.Update)
		r.Delete("/memory/{memoryID}", h.projectMemory.Delete)

		r.Delete("/layouts/{layoutID}", h.projectLayouts.Delete)
		r.Put("/layouts/{layoutID}", h.projectLayouts.Update)
		r.Post("/layouts/{layoutID}/restore", h.projectLayouts.Restore)
		r.Delete("/components/{componentID}", h.projectComponents.Delete)
		r.Put("/components/{componentID}", h.projectComponents.Update)
		r.Post("/components/{componentID}/restore", h.projectComponents.Restore)
		r.Post("/service-providers/{provID}/toggle", h.svcProviders.Toggle)
		r.Delete("/service-providers/{provID}", h.svcProviders.Delete)
	})
}

// registerEngineAndChatRoutes mounts the engine status, chat, and question-answer
// endpoints (all authenticated users; the destructive engine controls live in
// the admin-only group).
func registerEngineAndChatRoutes(r chi.Router, h *adminHandlers) {
	r.Get("/engine/{projectID}/status", h.engine.Status)
	r.Get("/chat/{projectID}/history", h.chat.History)

	// Chat send: synchronous JSON request/response.
	r.Post("/chat/{projectID}/stream", h.chat.Send)

	// Question answering, all authenticated users can answer.
	r.Post("/questions/{questionID}/answer", h.questions.Answer)
}

// registerAdminOnlyRoutes mounts the destructive / configuration endpoints. The
// caller wraps it in security.RequireRole("admin").
func registerAdminOnlyRoutes(r chi.Router, h *adminHandlers) {
	// Record every global admin mutation (users, providers, settings, engine
	// control, ...) to the audit trail. GET routes here, the audit log's own read
	// included, are skipped by the recorder.
	r.Use(h.audit.record)

	// Audit trail of control-plane mutations.
	r.Get("/system/audit", h.audit.List)

	// Projects (create, copy, import)
	r.Post("/projects", h.projects.Create)
	r.Post("/projects/{projectID}/copy", h.projects.Copy)
	r.Post("/projects/import", h.projectTransfer.Import)

	// Engine control. Interrupt sits with mode rather than with chat: it parks
	// the run and flips projects.mode, the same engine state the mode route owns.
	r.Post("/engine/{projectID}/start", h.engine.Start)
	r.Post("/engine/{projectID}/stop", h.engine.Stop)
	r.Post("/engine/{projectID}/interrupt", h.engine.Interrupt)
	r.Post("/engine/{projectID}/mode", h.engine.Mode)

	// Providers (create, update, delete, test)
	r.Post("/providers", h.providers.Create)
	r.Put("/providers/{providerID}", h.providers.Update)
	r.Delete("/providers/{providerID}", h.providers.Delete)
	r.Post("/providers/test", h.providers.TestConfig)
	r.Post("/providers/{providerID}/test", h.providers.Test)
	r.Post("/providers/{providerID}/models", h.providers.CreateModel)
	r.Put("/providers/{providerID}/models/{modelID}", h.providers.UpdateModel)
	r.Delete("/providers/{providerID}/models/{modelID}", h.providers.DeleteModel)
	r.Post("/providers/{providerID}/models/{modelID}/default", h.providers.SetDefaultModel)
	r.Post("/providers/{providerID}/models/{modelID}/probe", h.providers.ProbeModel)
	r.Post("/providers/{providerID}/probe-models", h.providers.ProbeProviderModels)
	r.Post("/providers/{providerID}/default", h.providers.SetDefault)
	r.Post("/providers/{providerID}/sync-models", h.providers.SyncModels)
	r.Post("/providers/sync-all", h.providers.SyncAll)

	// Users (create, update, delete)
	r.Post("/users", h.users.Create)
	r.Put("/users/{userID}", h.users.Update)
	r.Delete("/users/{userID}", h.users.Delete)

	// Guides are read-only platform content (repo packs) - no write routes.

	// Settings (read + update). Read is admin-only too: the settings
	// map includes secret values (e.g. the Telegram bot token), so it
	// must not be exposed to non-admin authenticated users.
	r.Get("/settings", h.settings.Get)
	r.Get("/system/logs", h.system.Logs)
	r.Get("/system/backup", h.system.Backup)
	r.Get("/system/config", h.system.Config)
	r.Put("/settings", h.settings.Update)

	// Scheduled backups: config, the files on disk, and a manual run.
	r.Get("/system/backups", h.backups.Get)
	r.Put("/system/backups", h.backups.Update)
	r.Post("/system/backups/run", h.backups.Run)
	r.Get("/system/backups/{name}", h.backups.Download)
	r.Delete("/system/backups/{name}", h.backups.Delete)

	// Chat-bot provider helpers
	r.Post("/telegram/test", h.bot.TestTelegram)
	r.Get("/telegram/status", h.bot.Status)
	r.Post("/telegram/token", h.bot.SetToken)
	r.Delete("/telegram/token", h.bot.Disconnect)
	r.Post("/telegram/pair", h.bot.Pair)
	r.Delete("/telegram/users/{userID}", h.bot.RemoveUser)
	r.Post("/telegram/test-message", h.bot.SendTest)

	// Local OS firewall helper (adds/removes the inbound allow-rule;
	// prompts for elevation on the host machine).
	r.Post("/firewall/allow", h.firewall.Allow)
	r.Post("/firewall/remove", h.firewall.Remove)

	// Network position (public IP, local IPs, NAT detection) for the Go Live
	// flow's DNS-record and port-forward guidance.
	r.Get("/system/network", h.hosting.Network)

	// Router port forwarding for 80/443 (UPnP, NAT-PMP, PCP; LAN only).
	r.Get("/system/portmap/status", h.hosting.PortmapStatus)
	r.Post("/system/portmap/enable", h.hosting.PortmapEnable)
	r.Post("/system/portmap/disable", h.hosting.PortmapDisable)
}

// networkDetector shares the IP-change watcher's detector when there is one,
// so both use the same router cache instead of discovering it twice.
func networkDetector(deps *Deps) *netinfo.Detector {
	if deps.NetWatcher != nil {
		return deps.NetWatcher.Detector()
	}
	return netinfo.NewDetector()
}

// registerAdminSPA serves the embedded admin SPA for all non-API routes, falling
// back to index.html so client-side routing works on deep links.
//
// http.FileServer over an embed.FS sets no ETag/Last-Modified (zero ModTime), so
// a content-hash ETag is attached for cheap 304 revalidation. Filenames are
// unhashed, so Cache-Control is no-cache (always revalidate), not a long max-age.
func registerAdminSPA(r chi.Router, adminFS fs.FS, version string) {
	if adminFS == nil {
		return
	}

	// Go's built-in MIME table has no .woff2 entry, so http.FileServer would
	// fall through to the OS table (the registry on Windows, /etc/mime.types
	// elsewhere) and serve the admin's self-hosted faces with a different
	// Content-Type per machine, or none at all. Registered here rather than in
	// an init() so it sits next to the file server that needs it.
	_ = mime.AddExtensionType(".woff2", "font/woff2")

	fileServer := http.FileServer(http.FS(adminFS))

	// etags memoizes the per-file ETag. The embedded FS never changes at runtime,
	// so each file is hashed at most once for the process lifetime.
	var etags sync.Map // name(string) -> etag(string)
	etagFor := func(name string) string {
		if v, ok := etags.Load(name); ok {
			return v.(string)
		}
		data, err := fs.ReadFile(adminFS, name)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(data)
		et := fmt.Sprintf(`"%s-%x"`, version, sum[:8])
		etags.Store(name, et)
		return et
	}

	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the requested file. If it doesn't exist,
		// fall back to index.html for SPA routing.
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}

		// Check if the file exists in the embedded FS.
		if _, err := fs.Stat(adminFS, path[1:]); err != nil {
			// File not found - serve index.html for SPA routing. Revalidate so a
			// new build's shell is picked up, but avoid re-downloading when unchanged.
			indexBytes, readErr := fs.ReadFile(adminFS, "index.html")
			if readErr != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(indexBytes)
			return
		}

		if et := etagFor(path[1:]); et != "" {
			w.Header().Set("ETag", et)
			w.Header().Set("Cache-Control", "no-cache")
			if r.Header.Get("If-None-Match") == et {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		fileServer.ServeHTTP(w, r)
	})
}
