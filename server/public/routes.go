package public

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"openfabrica/billing"
	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/actionexec"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/server/middleware"
)

// Deps holds all public handler dependencies.
type Deps struct {
	DB               *db.DB
	ProjectDBManager *db.ProjectDBManager
	Bus              *events.Bus
	WSHub            *wsHub
	LLMRegistry      *llm.Set
	Encryptor        *security.Encryptor
	JWTManager       *security.JWTManager
	Logger           *slog.Logger
	// DailyTokenBudgetFn returns the per-project daily token budget (0 =
	// unlimited). A server function's ctx.llm gates on it against the shared
	// unattended-spend tally. Optional (nil = no gate).
	DailyTokenBudgetFn func() int
	// LLMHTTPClient is the instance's shared LLM client and CallLLM the engine's
	// retry policy, both used by every model call these handlers make. Optional.
	LLMHTTPClient *http.Client
	CallLLM       llm.CallFunc

	// PublicCSP is the instance-default Content-Security-Policy for public HTML
	// responses. A project may override it (Handler.resolveCSP); empty disables
	// the header unless a project sets its own.
	PublicCSP string
}

// completeDeps is what a server function's ctx.llm needs (actionexec.Complete).
func (d *Deps) completeDeps() actionexec.CompleteDeps {
	cd := actionexec.CompleteDeps{Encryptor: d.Encryptor, HTTPClient: d.LLMHTTPClient, BudgetFn: d.DailyTokenBudgetFn, Call: d.CallLLM}
	if d.DB != nil {
		cd.GlobalDB = d.DB.DB
	}
	return cd
}

// RegisterRoutes mounts all public project routes on the given router.
func RegisterRoutes(r chi.Router, deps *Deps) {
	h := &Handler{
		deps:                deps,
		apiCache:            newAPICache(),
		authLimiter:         middleware.NewRateLimiter(middleware.PublicLoginRatePerSec, middleware.PublicLoginBurst),
		magicRequestLimiter: middleware.NewRateLimiter(middleware.MagicRequestRatePerSec, middleware.MagicRequestBurst),
		magicEmailLimiter:   middleware.NewRateLimiter(0.05, 3),
		magicVerifyLimiter:  middleware.NewRateLimiter(middleware.MagicVerifyRatePerSec, middleware.MagicVerifyBurst),
		totpVerifyLimiter:   middleware.NewRateLimiter(middleware.TOTPVerifyRatePerSec, middleware.TOTPVerifyBurst),
		authLockout:         middleware.NewAccountLockout(middleware.PublicLockoutAttempts, middleware.LockoutBaseDelay, middleware.LockoutMaxDelay),
	}

	// All public routes go through ProjectResolver which looks up the
	// project from the Host header and injects it into context.
	r.Use(h.ProjectResolver)

	// Block visitors on the project's IP/CIDR ban list (403) before any
	// content is served. Runs after ProjectResolver so the project is known.
	r.Use(h.IPBanCheck)

	// CSP runs after ProjectResolver so it can honor a per-project override,
	// falling back to the instance default. HTML responses only.
	r.Use(middleware.HTMLContentSecurityPolicy(h.resolveCSP))

	// SEO essentials.
	r.Get("/sitemap.xml", h.Sitemap)
	r.Get("/robots.txt", h.Robots)

	// PWA assets (served from DB content, not disk).
	r.Get("/manifest.json", h.ServeManifest)
	r.Get("/service-worker.js", h.ServeServiceWorker)

	// Unified artifact serving, CSS, JS, SVG, images, user uploads, and
	// platform-generated files (api-docs.json, etc.) all live under /files/.
	r.Get("/files/*", h.ServeFile)

	// Favicon fallback, browsers fetch /favicon.ico unconditionally, even
	// when the layout links a different file. ServeFavicon redirects to a
	// saved favicon.{ico,svg,png} under /files/. The .svg/.png roots are
	// served too: saved assets live only under /files/, so an engine-authored
	// service worker or <link> that references the conventional root path
	// (e.g. precaching "/favicon.svg") would otherwise 404 and, via the
	// atomic cache.addAll, silently break the whole offline install.
	r.Get("/favicon.ico", h.ServeFavicon)
	r.Get("/favicon.svg", h.ServeFavicon)
	r.Get("/favicon.png", h.ServeFavicon)

	// Page JSON API (SPA router navigation).
	r.Get("/api/page", h.Page)

	// Incoming webhooks.
	r.Post("/webhooks/{name}", h.IncomingWebhook)

	// Provider payment callbacks. Unauthenticated by design: the provider holds no
	// token, and the request's signature is what authenticates it. The path comes
	// from the payments package, which is also what tells the model to register it.
	r.Post(billing.WebhookPath, h.PaymentWebhook)

	// In-app notification inbox for the signed-in user (read side of
	// manage_notifications / the send_notification trigger).
	r.Get("/__of/notifications", h.NotificationsList)
	r.Get("/__of/notifications/count", h.NotificationsCount)
	r.Post("/__of/notifications/read-all", h.NotificationsReadAll)
	r.Post("/__of/notifications/{id}/read", h.NotificationsMarkRead)
	r.Delete("/__of/notifications/{id}", h.NotificationsDelete)

	// The stub checkout a user-flow replay pays on (404 outside a running replay).
	r.Get("/__of/checkout/{id}", h.FlowCheckoutPage)
	r.Post("/__of/checkout/{id}/{outcome}", h.FlowCheckoutComplete)

	// RSS / Atom feeds rendered from a table.
	r.Get("/feed/*", h.Feed)

	// Dynamic API endpoints (LLM-created), includes auth endpoints.
	r.HandleFunc("/api/*", h.DynamicAPI)

	// Catch-all: serve SSR pages by path.
	r.Get("/*", h.ServePage)
}
