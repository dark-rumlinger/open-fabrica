package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/server/admin"
	"openfabrica/server/deps"
	"openfabrica/server/middleware"
	"openfabrica/server/public"
)

// ServerDeps holds all dependencies needed by the HTTP servers. The shared
// set lives in deps.Core (also embedded by admin.Deps); only the fields no
// other server needs stay declared here.
type ServerDeps struct {
	deps.Core
	AdminFS fs.FS // embedded admin SPA filesystem
}

// Server manages the admin and public HTTP servers.
type Server struct {
	adminRouter  chi.Router
	publicRouter chi.Router
	deps         *ServerDeps
	wsHub        events.WSBroadcaster
}

// WSBroadcaster returns the WebSocket broadcaster, allowing external components
// (e.g. the Actions Runner) to push messages to connected WS clients.
func (s *Server) WSBroadcaster() events.WSBroadcaster {
	return s.wsHub
}

// isDynamicAPIPreflight reports whether a request targets the dynamic /api/*
// surface that owns its own CORS preflight (public.DynamicAPI). The public
// router's coarse CORS layer uses it to hand those OPTIONS requests downstream
// instead of answering them with the instance-wide policy. Kept in sync with the
// /api/* mount in public.RegisterRoutes.
func isDynamicAPIPreflight(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/")
}

// New creates and configures both routers.
func New(deps *ServerDeps) *Server {
	s := &Server{deps: deps}
	for _, err := range middleware.SetTrustedProxies(deps.Config.TrustedProxies) {
		slog.Warn("ignoring trusted_proxies entry", "error", err)
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// ---- Admin Router ----
	// No rate limiting on admin - it's a local admin tool, not a public API.
	s.adminRouter = chi.NewRouter()
	s.adminRouter.Use(middleware.RequestID)
	// A loopback-bound admin panel is only ever reached as localhost, so any other
	// Host is a relayed request (DNS rebinding, a same-host reverse proxy). An
	// exposed panel sits behind JWT auth on every route and is reached by whatever
	// name its operator chose, so the check applies only to the loopback case.
	if !deps.Config.AdminExposed() {
		s.adminRouter.Use(middleware.LoopbackHostOnly)
	}
	// Browsers tag cross-site writes; refuse them so a page on a sibling origin
	// cannot ride the admin session cookie.
	s.adminRouter.Use(middleware.RejectCrossSite)
	s.adminRouter.Use(middleware.Logging(logger.With("server", "admin")))
	s.adminRouter.Use(middleware.Recover(logger.With("server", "admin")))
	// The admin panel is a same-origin, localhost-only control plane, so its CORS
	// allowlist is fixed to the loopback admin origins. It is deliberately NOT
	// driven by cors_origins - that knob governs the public project APIs (see the
	// public router below). Cross-origin admin API access is not a supported
	// configuration; reach an exposed admin panel same-origin (via reverse proxy)
	// or over an SSH tunnel.
	adminCORS := []string{
		fmt.Sprintf("http://localhost:%d", deps.Config.AdminPort),
		fmt.Sprintf("http://127.0.0.1:%d", deps.Config.AdminPort),
	}
	s.adminRouter.Use(middleware.CORS(adminCORS))
	s.adminRouter.Use(middleware.Compress)
	s.adminRouter.Use(middleware.SecurityHeaders)
	s.adminRouter.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fully self-contained: every script, style, and font is served from
			// this binary, so the admin panel works offline and never makes a
			// third-party request (no CDN, no web fonts, no phone-home).
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'")
			next.ServeHTTP(w, r)
		})
	})

	// Translate API error messages into the language the admin UI reports. Last,
	// so it sits directly around the handlers; English requests pass straight through.
	s.adminRouter.Use(msgcat.Middleware)

	adminDeps := &admin.Deps{Core: deps.Core}
	adminDeps.Logger = logger.With("server", "admin")
	admin.RegisterRoutes(s.adminRouter, adminDeps, deps.AdminFS)

	// ---- Public Router ----
	publicRateLimiter := middleware.NewRateLimiter(deps.Config.RateLimitRate, deps.Config.RateLimitBurst)
	s.publicRouter = chi.NewRouter()
	s.publicRouter.Use(middleware.RequestID)
	s.publicRouter.Use(middleware.Logging(logger.With("server", "public")))
	s.publicRouter.Use(middleware.Recover(logger.With("server", "public")))
	// CSP is applied inside the public router, after ProjectResolver, so it can
	// honor a per-project override (public.RegisterRoutes / Handler.resolveCSP).
	// Public project APIs honor the instance-wide cors_origins allowlist; empty
	// (the default) means allow-all "*" with credentials withheld. Per-endpoint
	// CORS (public/cors.go) can narrow it further. The dynamic /api/* endpoints
	// answer their OWN preflight (handleAPIPreflight), so this coarse layer still
	// writes the instance-wide headers on /api/* but hands OPTIONS downstream
	// instead of short-circuiting it; every other route's preflight is answered here.
	s.publicRouter.Use(middleware.CORSWithPreflightPassthrough(deps.Config.CORSOrigins, isDynamicAPIPreflight))
	s.publicRouter.Use(publicRateLimiter.Handler)
	s.publicRouter.Use(middleware.Compress)
	s.publicRouter.Use(middleware.SecurityHeaders)

	hub := public.NewWSHub()
	s.wsHub = hub
	if deps.Bus != nil {
		deps.Bus.Subscribe(events.EventWSReducerChanged, func(e events.Event) {
			if path, _ := e.Payload["endpoint_path"].(string); path != "" {
				hub.ReloadEndpointReducer(e.ProjectID, path)
			}
		})
	}

	publicDeps := &public.Deps{
		DB:                 deps.DB,
		ProjectDBManager:   deps.ProjectDBManager,
		Bus:                deps.Bus,
		WSHub:              hub,
		LLMRegistry:        deps.LLMRegistry,
		Encryptor:          deps.Encryptor,
		JWTManager:         deps.JWTManager,
		Logger:             logger.With("server", "public"),
		PublicCSP:          deps.Config.PublicCSP,
		DailyTokenBudgetFn: deps.DailyTokenBudgetFn,
		LLMHTTPClient:      deps.LLMHTTPClient,
	}
	if deps.Engine != nil {
		publicDeps.CallLLM = deps.Engine.CallLLM
	}
	public.RegisterRoutes(s.publicRouter, publicDeps)

	return s
}

// withHealth answers liveness and readiness probes ahead of all router
// middleware so a probe always reaches a cheap handler regardless of Host
// header or project state. The paths are reserved on the public listener.
//
//   - GET /health, /healthz: liveness. Always 200; does NOT touch the database.
//   - GET /ready, /readyz: readiness. 200 only when ready() passes (a fast DB
//     ping); 503 otherwise. A nil ready degrades readiness to a liveness 200.
func withHealth(next http.Handler, ready func() error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/healthz") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/ready" || r.URL.Path == "/readyz") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if ready != nil {
				if err := ready(); err != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"status":"unavailable"}`))
					return
				}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readiness reports whether this instance can serve dependent traffic. It runs a
// short-deadline ping against the read pool; a failure (DB unreachable, pool
// exhausted past the deadline) surfaces as a 503 on /ready so orchestrators stop
// routing to this instance until it recovers.
func (s *Server) readiness() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.deps.DB.PingContext(ctx)
}

// Start starts both HTTP servers in goroutines. It blocks until the provided
// context is cancelled or an OS signal (SIGINT, SIGTERM) is received, then
// performs graceful shutdown.
func (s *Server) Start(ctx context.Context) error {
	// Admin binds to a specific interface (loopback by default) so exposing the
	// public server to a network does not also expose the control plane. Public
	// binds to all interfaces - it is meant to be reachable.
	adminAddr := net.JoinHostPort(s.deps.Config.AdminBindAddr, strconv.Itoa(s.deps.Config.AdminPort))
	publicAddr := fmt.Sprintf(":%d", s.deps.Config.PublicPort)

	adminServer := &http.Server{
		Addr:              adminAddr,
		Handler:           withHealth(s.adminRouter, s.readiness),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	publicServer := &http.Server{
		Addr:              publicAddr,
		Handler:           withHealth(s.publicRouter, s.readiness),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	lc := net.ListenConfig{Control: setReuseAddr}
	adminListener, err := lc.Listen(ctx, "tcp", adminAddr)
	if err != nil {
		return fmt.Errorf("listen admin %s: %w", adminAddr, err)
	}
	publicListener, err := lc.Listen(ctx, "tcp", publicAddr)
	if err != nil {
		_ = adminListener.Close()
		return fmt.Errorf("listen public %s: %w", publicAddr, err)
	}

	errCh := make(chan error, 2)

	go func() {
		s.deps.Logger.Debug("admin server starting", "addr", adminAddr)
		if err := adminServer.Serve(adminListener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("admin server: %w", err)
		}
	}()

	go func() {
		s.deps.Logger.Debug("public server starting", "addr", publicAddr)
		if err := publicServer.Serve(publicListener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("public server: %w", err)
		}
	}()

	// Wait for context cancellation, OS signal, or server error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
		s.deps.Logger.Info("context cancelled, shutting down servers")
	case sig := <-sigCh:
		s.deps.Logger.Info("received signal, shutting down servers", "signal", sig)
	case err := <-errCh:
		s.deps.Logger.Error("server error, shutting down", "error", err)
		return err
	}

	// Graceful shutdown with a shared deadline; run admin + public in parallel
	// so neither one's drain time eats into the other's.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	shutdownErrs := make(chan error, 2)
	go func() { shutdownErrs <- adminServer.Shutdown(shutdownCtx) }()
	go func() { shutdownErrs <- publicServer.Shutdown(shutdownCtx) }()

	var shutdownErr error
	for i := 0; i < 2; i++ {
		if err := <-shutdownErrs; err != nil {
			s.deps.Logger.Error("server shutdown error", "error", err)
			shutdownErr = err
		}
	}

	// Listeners are down, so no new WebSocket events arrive: drain the room actors
	// (authoritative endpoints) so each flushes its final state BEFORE main.go's
	// deferred ProjectDBManager.CloseAll runs. Otherwise an actor mid-event would
	// persist into a closed DB.
	if d, ok := s.wsHub.(interface{ DrainActors(context.Context) }); ok {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
		d.DrainActors(drainCtx)
		cancelDrain()
	}

	s.deps.Logger.Info("servers stopped")
	return shutdownErr
}
