//go:generate goversioninfo -64 -icon=openfabrica.ico

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/engine"
	_ "openfabrica/engine/validators" // register post-build validators via init()
	"openfabrica/internal/browseropen"
	"openfabrica/internal/firewall"
	"openfabrica/internal/logring"
	"openfabrica/internal/rotatelog"
	"openfabrica/internal/winclose"
	_ "openfabrica/providers/imagegen/automatic1111" // register image provider via init()
	_ "openfabrica/providers/imagegen/openai"        // register image provider via init()
	_ "openfabrica/providers/llm/anthropic"          // register provider via init()
	_ "openfabrica/providers/llm/ollama"             // register provider via init()
	_ "openfabrica/providers/llm/openai"             // register provider via init()
	_ "openfabrica/providers/stocksearch/pexels"     // register stock-search provider via init()
	_ "openfabrica/providers/stocksearch/pixabay"    // register stock-search provider via init()
	_ "openfabrica/providers/stocksearch/unsplash"   // register stock-search provider via init()
	_ "openfabrica/providers/websearch/brave"        // register web-search provider via init()
	_ "openfabrica/providers/websearch/tavily"       // register web-search provider via init()
	"openfabrica/tlsproxy"
	_ "openfabrica/tools/all" // register every tool category via init()
)

var Version = "0.2.4"

// BuildTime is the binary build timestamp, injected at link time via
// -X main.BuildTime=<RFC3339>. Empty in `go run` / dev builds. It is recorded on
// every run alongside Version (see engine.BuildVersion / engine.BuildBuiltAt) so a
// build export carries which binary produced it.
var BuildTime string

// parseArgs handles the informational command-line flags. It returns the config
// file path to load and whether main should stop early (help or version was
// printed). It is a small hand-rolled parser rather than the flag package so it
// never consumes the internal firewall subcommand args; an unknown argument
// prints an error and exits non-zero.
func parseArgs() (configPath string, done bool) {
	configPath = config.DefaultConfigPath
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help", a == "help":
			printUsage()
			return configPath, true
		case a == "-v", a == "--version", a == "version":
			built := BuildTime
			if built == "" {
				built = "dev"
			}
			fmt.Fprintf(os.Stdout, "Open Fabrica %s (built %s)\n", Version, built)
			return configPath, true
		case a == "-c", a == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --config requires a path argument")
				os.Exit(2)
			}
			i++
			configPath = args[i]
		case strings.HasPrefix(a, "--config="):
			configPath = strings.TrimPrefix(a, "--config=")
		default:
			fmt.Fprintf(os.Stderr, "error: unknown argument %q (try --help)\n", a)
			os.Exit(2)
		}
	}
	return configPath, false
}

// printUsage writes the command-line usage and the most common environment
// variables to stdout. The full set of settings lives in config.example.json
// and the README configuration table.
func printUsage() {
	fmt.Fprintf(os.Stdout, `Open Fabrica %s - self-hosted app builder and host.

Usage:
  openfabrica [flags]
  openfabrica backup [file.zip]            Write a backup of the whole instance (safe while running).
  openfabrica restore <file.zip> [--force] Restore a backup into an empty instance (stop the server first).
  openfabrica reset-admin [username]       Set a new random password for the admin (locked out? run this).
  openfabrica eval [--briefs <glob>]       Build a corpus of briefs on the running server and score each build.
  openfabrica eval compare <dirA> <dirB>   Compare two eval runs.

Flags:
  -c, --config <path>   Path to the JSON config file (default %q).
  -v, --version         Print version and build time, then exit.
  -h, --help            Print this help, then exit.

Configuration precedence: built-in defaults < config file < environment.
Common environment variables (config.example.json and the README list the full set):
  OPENFABRICA_ADMIN_PORT     Admin UI port (default %d).
  OPENFABRICA_PUBLIC_PORT    Public app port (default %d).
  OPENFABRICA_ADMIN_BIND     Admin bind address (default %s; routable needs OPENFABRICA_ADMIN_EXPOSE=1).
  OPENFABRICA_DATA_DIR       Data directory for the database and keys (default %s).
  OPENFABRICA_LOG_LEVEL      Log level: debug, info, warn, error (default %s).
  OPENFABRICA_NO_BROWSER     Set to 1 to not open a browser on start.

After starting, open the admin URL printed on the console to run first-time setup.
`, Version, config.DefaultConfigPath,
		config.DefaultAdminPort, config.DefaultPublicPort, config.DefaultAdminBindAddr,
		config.DefaultDataDir, config.DefaultLogLevel)
}

// applyMemoryLimit sets a soft heap ceiling via runtime/debug.SetMemoryLimit so a
// runaway model-authored server function pressures GC toward the limit instead of
// climbing until the OS OOM-kills the whole process. It is a floor, not tenant
// isolation: goja exposes no per-VM heap cap (see internal/funcsandbox), so this
// only buys headroom. The value comes from OPENFABRICA_MEMORY_LIMIT (e.g. "1GiB",
// "512MiB", or raw bytes; "0" disables it). When that is unset the Go runtime
// still honors a GOMEMLIMIT set in the environment; if neither is set the process
// runs without a soft ceiling and a one-line hint points at how to add one.
func applyMemoryLimit() {
	raw, ok := os.LookupEnv("OPENFABRICA_MEMORY_LIMIT")
	if !ok || strings.TrimSpace(raw) == "" {
		if _, envSet := os.LookupEnv("GOMEMLIMIT"); envSet {
			slog.Debug("soft memory ceiling taken from GOMEMLIMIT")
			return
		}
		slog.Info("no soft memory ceiling set: a runaway server function can exhaust RAM and take down the instance. " +
			"Set OPENFABRICA_MEMORY_LIMIT (e.g. 1GiB) or run under a container memory limit with auto-restart. See docs/SECURITY.md")
		return
	}
	limit, err := parseByteSize(raw)
	if err != nil {
		slog.Warn("ignoring invalid OPENFABRICA_MEMORY_LIMIT", "value", raw, "error", err)
		return
	}
	if limit <= 0 {
		debug.SetMemoryLimit(1<<63 - 1) // math.MaxInt64: the documented "no limit" value
		slog.Info("soft memory ceiling disabled via OPENFABRICA_MEMORY_LIMIT=0")
		return
	}
	debug.SetMemoryLimit(limit)
	slog.Info("soft memory ceiling applied", "bytes", limit)
}

// parseByteSize parses a human byte size: a bare number of bytes, or a number
// with a decimal (kB/MB/GB) or binary (KiB/MiB/GiB) unit. Case-insensitive.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num := strings.TrimSpace(s[:i])
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", num)
	}
	var mult float64
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1e3
	case "kib":
		mult = 1 << 10
	case "m", "mb":
		mult = 1e6
	case "mib":
		mult = 1 << 20
	case "g", "gb":
		mult = 1e9
	case "gib":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("unknown unit in %q", s)
	}
	return int64(f * mult), nil
}

// engineStageTimeouts maps the configured per-stage timeout minutes to the
// engine's stage-keyed override map. Only non-zero entries are included so each
// unset stage falls back to the engine's built-in default.
func engineStageTimeouts(cfg *config.Config) map[string]time.Duration {
	overrides := map[string]int{
		engine.StageExpand: cfg.EngineStageExpandTimeoutMin,
		engine.StagePlan:   cfg.EngineStagePlanTimeoutMin,
		engine.StageBuild:  cfg.EngineStageBuildTimeoutMin,
	}
	out := make(map[string]time.Duration)
	for stage, min := range overrides {
		if min > 0 {
			out[stage] = time.Duration(min) * time.Minute
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveEngineDailyTokenBudget resolves the per-project per-UTC-day engine
// token ceiling, in precedence order: (1) OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET
// env var, (2) the admin "engine_daily_token_budget" setting, (3)
// cfg.EngineDailyTokenBudget (config.json or the safe built-in default). A
// returned 0 means unlimited (explicit opt-out). Called at startup and on every
// EventSettingsChanged so the running engine tracks live edits without a restart.
func resolveEngineDailyTokenBudget(cfg *config.Config, database *db.DB) int {
	if v, ok := os.LookupEnv("OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	if database != nil {
		if v, err := database.GetSetting("engine_daily_token_budget"); err == nil && v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				return n
			}
		}
	}
	return cfg.EngineDailyTokenBudget
}

// warnIfExposedWithPermissiveDefaults emits a single loud startup warning when
// the instance is reachable on the internet (at least one active project has a
// public domain) but the public router still serves wide-open defaults: a
// wildcard CORS policy (empty cors_origins) and/or the permissive built-in CSP.
// Both are safe and intentional on a local-only box, so the check stays silent
// until a domain makes the surface public. It never blocks startup - the operator
// may have tightened these at a reverse proxy in front of the public port.
func warnIfExposedWithPermissiveDefaults(cfg *config.Config, tlsMgr *tlsproxy.Manager) {
	issues := permissiveDefaultIssues(tlsMgr.HasActiveDomains(), cfg.CORSOrigins, cfg.PublicCSP)
	if len(issues) == 0 {
		return
	}
	slog.Warn("public project server is internet-reachable with permissive defaults; "+
		"a stored XSS could then run inline script and read a visitor's auth_token from localStorage. "+
		"Tighten before serving untrusted or multi-user apps (see the README's \"Before You Go Live\")",
		"issues", issues,
		"fix", "set OPENFABRICA_PUBLIC_CSP=hardened (locks scripts to self+nonce; or set a per-project CSP) and cors_origins to your app's origins")
}

// permissiveDefaultIssues is the startup warning's view of
// config.PermissiveDefaultIssues, which the dashboard's live status also uses.
func permissiveDefaultIssues(hasActiveDomains bool, corsOrigins []string, publicCSP string) []string {
	return config.PermissiveDefaultIssues(hasActiveDomains, corsOrigins, publicCSP)
}

func printBanner(cfg *config.Config) {

	useColor := os.Getenv("NO_COLOR") == ""

	cyan := ""
	green := ""
	dim := ""
	reset := ""
	if useColor {
		cyan = "\033[36m"
		green = "\033[32m"
		dim = "\033[2m"
		reset = "\033[0m"
	}

	adminURL := fmt.Sprintf("http://localhost:%d", cfg.AdminPort)
	publicURL := fmt.Sprintf("http://localhost:%d", cfg.PublicPort)

	fmt.Fprintf(os.Stdout, `
%s    ██████╗ ███████╗
   ██╔═══██╗██╔════╝
   ██║   ██║█████╗
   ██║   ██║██╔══╝
   ╚██████╔╝██║
    ╚═════╝ ╚═╝     %s
   %sOpen Fabrica%s  %sv%s%s

   ─────────────────────────────────
   %sAdmin:%s     %s
   %sProjects:%s  %s
   ───────────────────────────────── 
   
   %sSpecial thanks to:
   https://letsencrypt.org

   Created by Dark Rumlinger
   https://github.com/dark-rumlinger/open-fabrica
   %s

`, cyan, reset, reset, reset, dim, Version, reset,
		green, reset, adminURL,
		green, reset, publicURL,
		dim, reset)
}

func main() {
	// Elevated firewall-helper child: this same binary is re-executed under UAC/
	// polkit with the apply-firewall subcommand. It must short-circuit before the
	// single-instance guard (the parent holds that) and any DB/server boot - it
	// only adds the firewall rule and exits. No-op on a normal startup.
	if firewall.RunSubcommandIfPresent() {
		return
	}

	// Informational command-line flags are handled after the firewall re-exec
	// check (that internal child passes its own subcommand args) and before any
	// startup work. configPath selects the file for config.LoadFrom in
	// bootstrapProcess.
	if handled, code := runSubcommand(os.Args[1:]); handled {
		os.Exit(code)
	}

	configPath, done := parseArgs()
	if done {
		return
	}

	cfg := bootstrapProcess(configPath)

	database, encryptor, jwtMgr, projectDBMgr := openDataLayer(cfg)
	defer func() { _ = database.Close() }()
	defer projectDBMgr.CloseAll()

	// Process-lifecycle context for background workers (engine, jobs, webhook
	// deliveries, trigger actions). Canceling it aborts blocked retries and
	// outbound HTTP on shutdown.
	engineCtx, engineCancel := context.WithCancel(context.Background())
	defer engineCancel()

	// Event-driven side effects (webhook deliveries, trigger actions) get their own
	// child context so shutdown can abort them first, before draining the workers.
	actionsCtx, actionsCancel := context.WithCancel(engineCtx)
	defer actionsCancel()

	bus, actionRunner := wireEventBus(projectDBMgr, encryptor, actionsCtx)

	llmRegistry, toolRegistry, toolExecutor := initRegistries(database, actionRunner)

	llmHTTPClient, budgetFn, dailyTokenBudget := initLLMClientAndBudget(cfg, database, encryptor, llmRegistry, actionRunner)

	app := &appDeps{
		cfg:           cfg,
		database:      database,
		projectDBMgr:  projectDBMgr,
		encryptor:     encryptor,
		jwtMgr:        jwtMgr,
		bus:           bus,
		llmRegistry:   llmRegistry,
		toolRegistry:  toolRegistry,
		llmHTTPClient: llmHTTPClient,
		budgetFn:      budgetFn,
		toolExecutor:  toolExecutor,
	}
	app.toolEnv = newToolEnv(app)
	// The trigger runner's messaging actions (send_email/push) dispatch through
	// manage_email/push instead of reimplementing provider auth and shaping.
	actionRunner.SetTools(toolExecutor, app.toolEnv)

	engineMgr, jobsMgr := newEngineAndJobs(engineCtx, app)
	// Trigger llm_call actions ride out transient provider failures under the
	// engine's retry policy.
	actionRunner.SetLLMCaller(app.callLLM)

	chatHandler := newChatHandler(app)
	defer chatHandler.Close()

	logFirstRunStatus(database)

	bootWarnings, botService := startBackgroundWorkers(engineCtx, cfg, database, projectDBMgr, engineMgr, jobsMgr, bus, encryptor, dailyTokenBudget)

	tlsMgr := startTLSProxy(cfg, database, bus, jwtMgr)
	netHelpers := startNetworkHelpers(cfg, database)
	wireSelfDestruct(selfDestructDeps{
		botService:    botService,
		cfg:           cfg,
		configPath:    configPath,
		cancelActions: actionsCancel,
		tlsMgr:        tlsMgr,
		netHelpers:    netHelpers,
		engineMgr:     engineMgr,
		jobsMgr:       jobsMgr,
		database:      database,
		projectDBMgr:  projectDBMgr,
	})

	srv := newServer(app, engineMgr, jobsMgr, chatHandler, tlsMgr, netHelpers, actionRunner, bootWarnings, botService)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Windows-only: catch CTRL_CLOSE_EVENT (X button), logoff, shutdown.
	// signal.NotifyContext above does not see those on Windows.
	if err := winclose.NotifyOnClose(cancel); err != nil {
		slog.Debug("console close handler not registered", "error", err)
	}

	serveErr := srv.Start(ctx)
	if serveErr != nil {
		slog.Error("server error", "error", serveErr)
	}

	gracefulShutdown(actionsCancel, tlsMgr, netHelpers, engineMgr, jobsMgr)

	// A listener that never came up (port in use, bad bind address) must not look
	// like a clean stop, or restart-on-failure supervisors will not retry.
	if serveErr != nil {
		os.Exit(1)
	}
}

// openBrowserWhenReady polls the admin TCP port until it accepts a
// connection, then launches the default browser. Runs in a goroutine so it
// never blocks server startup. Gives up silently after the deadline; failures
// are logged at debug since a headless host is a normal mode of operation.
func openBrowserWhenReady(adminPort int, url string) {
	addr := fmt.Sprintf("127.0.0.1:%d", adminPort)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			if err := browseropen.Open(url); err != nil {
				slog.Debug("browser not opened", "reason", err)
			} else {
				slog.Debug("opened browser", "url", url)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	slog.Debug("browser not opened", "reason", "admin port did not become ready within 10s")
}

func initLogger(level string) { initLoggerWithFile(level, "") }

// logFileSize and logFileKeep size the rotating log file.
const (
	logFileSize = 20 << 20
	logFileKeep = 5
)

// initLoggerWithFile logs to the console and to the in-memory ring the admin log
// view reads, and also to a rotated file when logFile is set.
func initLoggerWithFile(level, logFile string) {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	writers := []io.Writer{os.Stdout, logring.Default}
	if logFile != "" {
		if w, err := rotatelog.Open(logFile, logFileSize, logFileKeep); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot open log file %s: %v\n", logFile, err)
		} else {
			writers = append(writers, w)
		}
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: logLevel,
	})
	slog.SetDefault(slog.New(handler))
}
