package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"openfabrica/bot"
	"openfabrica/config"
	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/backup"
	"openfabrica/internal/httpx"
	"openfabrica/internal/logring"
	"openfabrica/internal/sqliteretry"
)

// SystemHandler handles system status and SSE event stream.
type SystemHandler struct {
	deps *Deps
}

type systemStatus struct {
	Version       string `json:"version"`
	GoVersion     string `json:"go_version"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	NumGoroutines int    `json:"num_goroutines"`
	UserCount     int    `json:"user_count"`
	ProjectCount  int    `json:"project_count"`
	ProviderCount int    `json:"provider_count"`
	BrainRunning  int    `json:"brain_running"`
	// BackgroundCommandsDropped is a running total of engine commands rejected
	// because a worker's channel was full. Non-zero values indicate sustained
	// backpressure (a stalled LLM call, a hot scheduled-task loop), so the UI
	// can surface "worker busy" instead of looking idle.
	BackgroundCommandsDropped uint64 `json:"background_commands_dropped"`
	// SQLiteBusyRetries / SQLiteBusyExhausted expose how often the
	// application-level retry wrapper around SQLite BUSY actually fires. With
	// busy_timeout=30s and a single-writer pool, both should stay near zero.
	// Sustained growth means real contention - investigate before the wrapper
	// runs out of retries and surfaces "database is locked" to users.
	SQLiteBusyRetries   uint64 `json:"sqlite_busy_retries"`
	SQLiteBusyExhausted uint64 `json:"sqlite_busy_exhausted"`
	Uptime              string `json:"uptime"`
	PublicPort          int    `json:"public_port"`
	// TLSDisabled tells the UI whether the built-in HTTPS proxy is off, so
	// public-domain links render as http:// instead of a dead https:// URL.
	TLSDisabled bool `json:"tls_disabled"`
	// Bot exposes the messaging bot's health (last poll, backoff, counters) so
	// the UI can show "bot healthy / last seen Xs ago" or surface a bad token.
	Bot bot.Stats `json:"bot"`
	// BootWarnings lists non-fatal startup failures (e.g. engine auto-start)
	// so an operator learns about them from the dashboard, not just from one
	// log line at boot. Empty on a clean start.
	BootWarnings []string `json:"boot_warnings,omitempty"`
	// ExposureWarnings lists wide-open public defaults (wildcard CORS, the permissive
	// CSP) that matter once a project has a public domain. Computed per request, not
	// at boot, so adding a domain from the admin UI raises it without a restart.
	ExposureWarnings []string `json:"exposure_warnings,omitempty"`
}

var startTime = time.Now()

// Status returns system status information for the admin dashboard.
func (h *SystemHandler) Status(w http.ResponseWriter, r *http.Request) {
	userCount, _ := models.CountUsers(h.deps.DB.DB)
	projects, _ := models.ListProjects(h.deps.DB.DB)
	providerCount, _ := models.CountProviders(h.deps.DB.DB)

	busyRetried, busyExhausted := sqliteretry.Stats()
	status := systemStatus{
		Version:                   h.deps.Version,
		GoVersion:                 runtime.Version(),
		GOOS:                      runtime.GOOS,
		GOARCH:                    runtime.GOARCH,
		NumGoroutines:             runtime.NumGoroutine(),
		UserCount:                 userCount,
		ProjectCount:              len(projects),
		ProviderCount:             providerCount,
		BrainRunning:              h.deps.Engine.RunningCount(),
		BackgroundCommandsDropped: engine.DroppedBackgroundCommands(),
		SQLiteBusyRetries:         busyRetried,
		SQLiteBusyExhausted:       busyExhausted,
		Uptime:                    time.Since(startTime).Round(time.Second).String(),
		PublicPort:                h.deps.Config.PublicPort,
		TLSDisabled:               h.deps.Config.TLSDisabled || h.deps.TLSManager == nil,
		Bot:                       bot.GetStats(),
		BootWarnings:              localizeAll(r, h.deps.BootWarnings),
	}
	if h.deps.TLSManager != nil {
		status.ExposureWarnings = localizeAll(r, config.PermissiveDefaultIssues(h.deps.TLSManager.HasActiveDomainsQuiet(), h.deps.Config.CORSOrigins, h.deps.Config.PublicCSP))
	}

	writeJSON(w, http.StatusOK, status)
}

// EventsBackfill returns events buffered since the given event ID. Clients
// call this after an SSE reconnect to recover events emitted during the
// disconnect window. The response includes:
//   - events: ordered list of events with ID > since
//   - oldest_id: smallest event ID currently retained
//   - missed: true when `since` is older than `oldest_id`, meaning the
//     buffer rolled and events between `since` and `oldest_id` are lost
//
// Optional ?project_id= filters to a single project (useful for chat view).
func (h *SystemHandler) EventsBackfill(w http.ResponseWriter, r *http.Request) {
	var since uint64
	if s := r.URL.Query().Get("since"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be a non-negative integer")
			return
		}
		since = v
	}

	var projectIDFilter int
	if s := r.URL.Query().Get("project_id"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "project_id must be a non-negative integer")
			return
		}
		projectIDFilter = v
	}

	all := h.deps.Bus.Since(since)
	out := all
	if projectIDFilter > 0 {
		out = make([]events.Event, 0, len(all))
		for _, e := range all {
			if e.ProjectID == projectIDFilter {
				out = append(out, e)
			}
		}
	}

	oldest := h.deps.Bus.OldestID()
	missed := since > 0 && oldest > 0 && since < oldest-1

	writeJSON(w, http.StatusOK, map[string]any{
		"events":    out,
		"oldest_id": oldest,
		"missed":    missed,
	})
}

// EventStream sends server-sent events for all system events.
func (h *SystemHandler) EventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Disable write deadline for this long-lived SSE connection.
	// Without this, Go's http.Server WriteTimeout kills the connection.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // best-effort; some writers don't support deadlines

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Channel to receive events.
	eventCh := make(chan events.Event, 64)

	// Subscribe to all events; clean up on disconnect.
	subID := h.deps.Bus.SubscribeAll(func(e events.Event) {
		select {
		case eventCh <- e:
		default:
			// Drop event if channel full to avoid blocking.
		}
	})

	// Send keepalive comment every 30 seconds.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			h.deps.Bus.Unsubscribe(subID)
			return
		case e := <-eventCh:
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			// id: line lets clients track the last-seen event for backfill on
			// reconnect via /admin/api/brain/events?since=N.
			if e.ID > 0 {
				fmt.Fprintf(w, "id: %d\n", e.ID)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// effectiveConfig is the read-only view of the running configuration an operator
// needs on launch day: where the data lives, what is exposed, and the limits in
// force. It is an explicit allowlist, so the JWT secret and encryption key (and
// any field added to config.Config later) never reach the client by accident.
type effectiveConfig struct {
	AdminPort      int      `json:"admin_port"`
	AdminBindAddr  string   `json:"admin_bind_addr"`
	AdminExposed   bool     `json:"admin_exposed"`
	PublicPort     int      `json:"public_port"`
	DataDir        string   `json:"data_dir"`
	DBPath         string   `json:"db_path"`
	LogLevel       string   `json:"log_level"`
	LogFile        string   `json:"log_file"`
	TLSDisabled    bool     `json:"tls_disabled"`
	TLSStaging     bool     `json:"tls_staging"`
	CORSOrigins    []string `json:"cors_origins"`
	TrustedProxies []string `json:"trusted_proxies"`
	RateLimitRate  float64  `json:"rate_limit_rate"`
	RateLimitBurst int      `json:"rate_limit_burst"`
	// PublicCSP names the policy rather than echoing it: "default", "hardened",
	// "off" (empty), or "custom".
	PublicCSP              string `json:"public_csp"`
	LLMTimeoutSec          int    `json:"llm_timeout_sec"`
	EngineDailyTokenBudget int    `json:"engine_daily_token_budget"`
	StageTimeoutsMin       struct {
		Expand int `json:"expand"`
		Plan   int `json:"plan"`
		Build  int `json:"build"`
	} `json:"stage_timeouts_min"`
}

// Config returns the effective configuration (admin only). Values come from the
// config file, overridden by environment variables; changing one means editing
// config.json or the environment and restarting.
//
//	GET /admin/api/system/config
func (h *SystemHandler) Config(w http.ResponseWriter, r *http.Request) {
	c := h.deps.Config
	out := effectiveConfig{
		AdminPort: c.AdminPort, AdminBindAddr: c.AdminBindAddr, AdminExposed: c.AdminExposed(),
		PublicPort: c.PublicPort, DataDir: c.DataDir, DBPath: c.DBPath,
		LogLevel: c.LogLevel, LogFile: c.LogFile,
		TLSDisabled: c.TLSDisabled, TLSStaging: c.TLSStaging,
		CORSOrigins: c.CORSOrigins, TrustedProxies: c.TrustedProxies,
		RateLimitRate: c.RateLimitRate, RateLimitBurst: c.RateLimitBurst,
		LLMTimeoutSec: c.LLMTimeoutSec, EngineDailyTokenBudget: c.EngineDailyTokenBudget,
	}
	switch c.PublicCSP {
	case "":
		out.PublicCSP = "off"
	case config.DefaultPublicCSP:
		out.PublicCSP = "default"
	case config.HardenedPublicCSP:
		out.PublicCSP = "hardened"
	default:
		out.PublicCSP = "custom"
	}
	out.StageTimeoutsMin.Expand = c.EngineStageExpandTimeoutMin
	out.StageTimeoutsMin.Plan = c.EngineStagePlanTimeoutMin
	out.StageTimeoutsMin.Build = c.EngineStageBuildTimeoutMin
	if out.CORSOrigins == nil {
		out.CORSOrigins = []string{}
	}
	if out.TrustedProxies == nil {
		out.TrustedProxies = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}

// Backup streams a whole-instance archive (core database, every project database,
// files, keys, ACME state) as a zip download. SQLite files are captured with
// VACUUM INTO, so it is safe on a running server. Restoring is deliberately not
// exposed: it replaces files the server holds open, so it stays a stopped-server
// CLI step (`openfabrica restore`). Admin only: the archive holds the encryption
// and signing keys.
//
//	GET /admin/api/system/backup
func (h *SystemHandler) Backup(w http.ResponseWriter, r *http.Request) {
	// Streaming a large instance outlives the blanket 120 s write timeout.
	httpx.ExtendIO(w, 30*time.Minute)

	name := fmt.Sprintf("openfabrica-backup-%s.zip", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")

	m, err := backup.Create(w, backup.Options{
		DataDir: h.deps.Config.DataDir,
		DBPath:  h.deps.Config.DBPath,
		Version: h.deps.Version,
	})
	if err != nil {
		// Headers and part of the body are already sent, so the status cannot
		// change; the truncated zip fails to open, which is the client's signal.
		h.deps.Logger.Error("backup download failed", "error", err)
		return
	}
	h.deps.Logger.Info("backup downloaded", "projects", len(m.Projects), "files", m.Files)
}

// Logs returns the most recent server log lines (?lines=N, default 200, max 2000)
// from the in-memory ring, newest last. Admin only: log lines can name hosts,
// projects and error text from providers.
func (h *SystemHandler) Logs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 {
		n = min(v, logring.DefaultLines)
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": logring.Default.Last(n)})
}
