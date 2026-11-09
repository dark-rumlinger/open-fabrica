// Package deps holds Core, the dependency bundle shared by the HTTP servers.
// server.ServerDeps and admin.Deps embed it, so adding a shared dependency is
// one field here instead of a hand-copied edit in every Deps struct.
//
// public.Deps deliberately does NOT embed Core: its narrowness is a security
// boundary - public handlers must not gain field-level access to the Engine,
// tool registry, or admin config just because embedding is convenient.
package deps

import (
	"log/slog"
	"net/http"

	"openfabrica/bot"
	"openfabrica/chat"
	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/firewall"
	"openfabrica/internal/netinfo"
	"openfabrica/internal/portmap"
	"openfabrica/jobs"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/tlsproxy"
	"openfabrica/tools"
)

// Core is the dependency set every server shares. Logger is per-server: the
// embedding side overwrites it with a labeled child (logger.With("server", ...)).
type Core struct {
	Config           *config.Config
	DB               *db.DB
	ProjectDBManager *db.ProjectDBManager
	JWTManager       *security.JWTManager
	Encryptor        *security.Encryptor
	Bus              *events.Bus
	Engine           *engine.Engine
	Bot              *bot.BotService
	Jobs             *jobs.Manager
	ChatHandler      *chat.ChatHandler
	LLMRegistry      *llm.Set
	ToolRegistry     *tools.Registry
	LLMHTTPClient    *http.Client
	// DailyTokenBudgetFn returns the live instance-wide daily token budget for
	// unattended AI (0 = unlimited), resolved once in main (env > setting > config).
	DailyTokenBudgetFn func() int
	Logger             *slog.Logger
	Version            string
	TLSManager         *tlsproxy.Manager
	FirewallManager    firewall.Manager
	// PortMapper forwards 80/443 on the router when the owner opts in, and
	// NetWatcher flags public domains whose DNS no longer points here. Both
	// may be nil (tests, minimal setups); handlers fall back gracefully.
	PortMapper *portmap.Mapper
	NetWatcher *netinfo.Watcher
	// BootWarnings carries non-fatal startup failures (engine/jobs AutoStart)
	// that would otherwise only exist as one boot-time log line. Surfaced via
	// the admin system status endpoint so the dashboard can show them.
	BootWarnings []string
}
