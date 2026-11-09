package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"openfabrica"
	"openfabrica/bot"
	"openfabrica/chat"
	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/backupsched"
	"openfabrica/internal/firewall"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/netinfo"
	"openfabrica/internal/portmap"
	"openfabrica/internal/singleinstance"
	"openfabrica/internal/tokenrevoke"
	"openfabrica/jobs"
	"openfabrica/providers/imagegen"
	"openfabrica/providers/llm"
	"openfabrica/providers/stocksearch"
	"openfabrica/providers/websearch"
	"openfabrica/security"
	"openfabrica/server"
	"openfabrica/server/deps"
	"openfabrica/store"
	"openfabrica/tlsproxy"
	"openfabrica/tools"
	"openfabrica/triggers"
	"openfabrica/webhooks"
)

// bootstrapProcess runs the process-level startup that must happen before any
// store or server exists: the logger, the build stamp, the single-instance
// guard, the config load, the banner, and the soft memory ceiling. Fatal
// problems exit the process directly.
func bootstrapProcess(configPath string) *config.Config {
	initLogger("info") // init early so config.LoadFrom() logs use the correct format

	// Stamp every run with the binary that produced it (surfaced in the run export's
	// `build` block) so a post-mortem reads the engine version/build-time as a field
	// instead of diffing the binary's mtime against source.
	engine.BuildVersion = Version
	engine.BuildBuiltAt = BuildTime

	// Refuse to start if another copy is already running. Without this, a
	// second double-click silently brings up a second console window and a
	// second browser tab - both processes compete for the same database and
	// admin port (SO_REUSEADDR on Windows allows duplicate binds).
	if err := singleinstance.Acquire("OpenFabrica-SingleInstance"); err != nil {
		fmt.Fprintln(os.Stderr, "Open Fabrica is already running. Check your taskbar or existing console window.")
		slog.Error("single-instance guard refused start", "error", err)
		time.Sleep(3 * time.Second) // keep the console visible long enough to read
		os.Exit(1)
	}

	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	printBanner(cfg)
	logFile := cfg.LogFile
	if logFile != "" && !filepath.IsAbs(logFile) {
		logFile = filepath.Join(cfg.DataDir, logFile)
	}
	initLoggerWithFile(cfg.LogLevel, logFile)

	// Apply a soft heap ceiling before booting the DB and server so it governs the
	// whole process, including model-authored server functions (goja has no per-VM
	// heap cap of its own).
	applyMemoryLimit()

	return cfg
}

// openDataLayer opens the global database and constructs the security
// primitives and the per-project database manager, in that order. The caller
// owns the returned handles: main defers database.Close and
// projectDBMgr.CloseAll so shutdown ordering stays visible in one place.
func openDataLayer(cfg *config.Config) (*db.DB, *security.Encryptor, *security.JWTManager, *db.ProjectDBManager) {
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}

	// Platform knowledge (the mental model, per-stage frames, capability guides,
	// and contracts) is served from the embedded knowledge tree (pack.Embedded
	// reads knowledge/); there is no DB overlay and no platform seeding step. See
	// packages knowledge and pack.

	encryptor, err := security.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		slog.Error("failed to init encryptor", "error", err)
		os.Exit(1)
	}

	jwtMgr := security.NewJWTManager(cfg.JWTSecret, 24*time.Hour)
	// SecureCookies is set later once we know whether the TLS proxy is
	// actually running (see startTLSProxy).

	projectDBMgr := db.NewProjectDBManager(cfg.DataDir)
	// Say which folder this run uses, so starting from the wrong working directory
	// (a fresh, empty instance) is visible instead of silent.
	slog.Info("data directory", "path", cfg.DataDir)

	return database, encryptor, jwtMgr, projectDBMgr
}

// wireEventBus constructs the process-wide event bus and the runners that act
// on its events: the webhook dispatcher, the trigger action runner, and the
// audit logger. The dispatcher and audit logger self-register; the action runner
// sends trigger_webhook through the dispatcher and is returned for later wiring.
func wireEventBus(projectDBMgr *db.ProjectDBManager, encryptor *security.Encryptor, engineCtx context.Context) (*events.Bus, *triggers.Runner) {
	bus := events.NewBus()
	dispatcher := webhooks.NewDispatcher(projectDBMgr, bus, encryptor, engineCtx) // Self-registers on the event bus via side effects.
	actionRunner := triggers.NewRunner(projectDBMgr, bus, encryptor, engineCtx)   // Server-side actions (email, HTTP, data) on events.
	actionRunner.SetWebhookSender(dispatcher)
	triggers.NewAuditLogger(projectDBMgr, bus) // Audit logging for security/data events.
	return bus, actionRunner
}

// initRegistries wires the LLM registry into the action runner,
// drains init()-time registration errors, validates the tool registry, and
// probes for headless Chrome. A registration or validation failure exits the
// process.
func initRegistries(database *db.DB, actionRunner *triggers.Runner) (*llm.Set, *tools.Registry, *tools.Executor) {
	llmRegistry := llm.NewRegistry()
	toolRegistry := tools.DefaultRegistry()
	// Wire the LLM registry + global DB into the trigger runner so
	// trigger_type=llm_call / ai_embed can resolve a provider at fire time.
	actionRunner.SetLLMRegistry(llmRegistry, database.DB)
	// Drain registration errors first: a duplicate name or missing metadata in
	// a tool's init() would otherwise be a single panic, hiding any sibling
	// errors. Same treatment for the provider registries: every problem is
	// reported before exit.
	if regErrs := collectRegistrationErrors(); len(regErrs) > 0 {
		os.Exit(1)
	}
	if warns := toolRegistry.OneLinerWarnings(engine.FallbackReadOnlyActions()); len(warns) > 0 {
		for _, w := range warns {
			slog.Warn("tool registry read-only fallback ambiguity", "warning", w)
		}
	}
	// Validate the registry once every tool has registered and any registration
	// failures have been reported. Catches drift (missing param descriptions,
	// type-mismatched examples, duplicate action names) before the first LLM call.
	// Fatal: a misregistered tool is a programming error that must not run.
	// Ordered after collectRegistrationErrors so a tool that failed to register is
	// reported as that, not as whatever validation notices about its absence.
	if err := toolRegistry.Validate(); err != nil {
		slog.Error("tool registry validation failed", "error", err)
		os.Exit(1)
	}
	toolExecutor := tools.NewExecutor(toolRegistry)

	slog.Debug("tools registered", "count", toolRegistry.Count())

	// External-binary probe. One INFO line so operators can see at a glance
	// whether headless Chrome is reachable, without grepping for runtime
	// errors from PDF/scrape tools. Deliberately INFO (not WARN) when
	// missing: for many installs the omission is a conscious choice
	// (headless server with no PDF use, etc.).
	if p := store.DetectChromePath(); p != "" {
		slog.Info("headless chrome detected, PDF and scrape tools enabled", "path", p)
	} else {
		slog.Info("headless chrome not found, PDF and scrape tools will error at use (install Google Chrome, Chromium, or Edge to enable)")
	}

	return llmRegistry, toolRegistry, toolExecutor
}

// llmIdleConnsPerHost is how many idle connections the shared LLM client keeps per
// provider host. Sized to the engine's per-model concurrency cap so a warm socket is
// waiting for every conversation in flight, with headroom for the non-engine callers
// (chat, jobs, triggers) that share this client.
const llmIdleConnsPerHost = 16

// initLLMClientAndBudget builds the shared LLM HTTP client, loads the optional
// provider seed, and sets up the live daily-token-budget resolver shared by
// every unattended LLM path. Returns the client, the budget resolver, and the
// atomic the settings-change subscriber writes to.
func initLLMClientAndBudget(cfg *config.Config, database *db.DB, encryptor *security.Encryptor, llmRegistry *llm.Set, actionRunner *triggers.Runner) (*http.Client, func() int, *atomic.Int64) {
	// OPENFABRICA_LLM_TIMEOUT sets this client's socket timeout, and the client IS the
	// effective per-call deadline for every LLM call (see Deps.LLMHTTPClient). A stage's
	// SessionSpec.Timeout is a looser outer bound on top: BUILD's is 90 minutes, well
	// past this, so on that stage the socket fires first and the retry classifier sees a
	// transport error rather than a context deadline. Both are retried, so the ordering
	// only changes which code the retry budget bills it to.
	llmTimeout := cfg.LLMTimeout()
	// Own transport rather than http.DefaultTransport, whose MaxIdleConnsPerHost of 2
	// would leave most connections un-pooled: the engine runs up to
	// MaxConcurrentPerModel conversations against one provider host, and every call
	// past the second would re-dial TCP and TLS.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = llmIdleConnsPerHost
	transport.MaxIdleConns = llmIdleConnsPerHost * 4
	llmHTTPClient := &http.Client{Timeout: llmTimeout + 30*time.Second, Transport: transport}

	if err := llm.LoadSeed(cfg.SeedPath, openfabrica.SeedJSON, database.DB, encryptor, llmRegistry, llmHTTPClient); err != nil {
		slog.Warn("provider seed not applied", "error", err)
	}

	// Engine daily token budget is resolved live (env > admin/wizard setting >
	// config/default) and held in an atomic so edits from the admin UI or setup
	// wizard apply to the running engine without a restart - see the
	// EventSettingsChanged subscriber in startBackgroundWorkers.
	dailyTokenBudget := new(atomic.Int64)
	dailyTokenBudget.Store(int64(resolveEngineDailyTokenBudget(cfg, database)))
	// One shared budget resolver for every unattended LLM path (engine, event
	// triggers, scheduled jobs) so the daily AI spend limit is a single pool, not
	// an engine-only cap. Reads the live atomic so admin/wizard edits apply
	// without a restart.
	budgetFn := func() int { return int(dailyTokenBudget.Load()) }
	actionRunner.SetBudgetFn(budgetFn)
	actionRunner.SetLLMHTTPClient(llmHTTPClient)

	return llmHTTPClient, budgetFn, dailyTokenBudget
}

// appDeps bundles the process-wide singletons the later wiring phases share,
// so their signatures stay within the linter's argument limit.
type appDeps struct {
	cfg           *config.Config
	database      *db.DB
	projectDBMgr  *db.ProjectDBManager
	encryptor     *security.Encryptor
	jwtMgr        *security.JWTManager
	bus           *events.Bus
	llmRegistry   *llm.Set
	toolRegistry  *tools.Registry
	llmHTTPClient *http.Client
	budgetFn      func() int
	toolExecutor  *tools.Executor
	toolEnv       *tools.Env
	// callLLM is the engine's call policy (retry + shared per-model limiter),
	// set once the engine exists and handed to chat, jobs, and triggers.
	callLLM llm.CallFunc
}

// newToolEnv builds the one tools.Env every tool caller shares (engine, live chat,
// scheduled jobs, event triggers), so each shapes an identical ToolContext.
func newToolEnv(a *appDeps) *tools.Env {
	return &tools.Env{
		GlobalDB:   a.database.DB,
		Bus:        a.bus,
		Encryptor:  a.encryptor,
		JWTManager: a.jwtMgr,
		PublicPort: a.cfg.PublicPort,
		DataDir:    a.projectDBMgr.DataDir(),
		EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
			return llm.EmbedTexts(ctx, a.database.DB, a.encryptor, a.llmRegistry, texts)
		},
	}
}

// newEngineAndJobs constructs the engine and jobs managers on the shared
// process-lifecycle context and registers the event-bus wake subscribers that
// drive them.
func newEngineAndJobs(engineCtx context.Context, a *appDeps) (*engine.Engine, *jobs.Manager) {
	engineDeps := &engine.Deps{
		DB:                 a.database,
		ProjectDBManager:   a.projectDBMgr,
		Encryptor:          a.encryptor,
		JWTManager:         a.jwtMgr,
		LLMRegistry:        a.llmRegistry,
		ToolRegistry:       a.toolRegistry,
		ToolExecutor:       a.toolExecutor,
		ToolEnv:            a.toolEnv,
		Bus:                a.bus,
		LLMHTTPClient:      a.llmHTTPClient,
		DailyTokenBudgetFn: a.budgetFn,
		StageTimeouts:      engineStageTimeouts(a.cfg),
		PublicPort:         a.cfg.PublicPort,
		PublicCSP:          a.cfg.PublicCSP,
	}
	engineMgr := engine.New(engineDeps, engineCtx)
	a.callLLM = engineMgr.CallLLM

	jobsMgr := jobs.NewManager(&jobs.Deps{
		DB:               a.database,
		ProjectDBManager: a.projectDBMgr,
		Encryptor:        a.encryptor,
		ToolExecutor:     a.toolExecutor,
		ToolEnv:          a.toolEnv,
		BudgetFn:         a.budgetFn,
		CallLLM:          a.callLLM,
		LLMHTTPClient:    a.llmHTTPClient,
	}, engineCtx)

	wireEngineWakeSubscribers(a.bus, engineMgr, jobsMgr)

	return engineMgr, jobsMgr
}

// newChatHandler constructs the chat session handler with its dependencies.
// The caller defers Close from main.
func newChatHandler(a *appDeps) *chat.ChatHandler {
	chatDeps := chat.SessionDeps{
		DB:               a.database.DB,
		ProjectDBManager: a.projectDBMgr,
		Bus:              a.bus,
		Logger:           slog.Default().With("component", "chat"),
	}
	return chat.NewChatHandler(chatDeps)
}

// logFirstRunStatus counts users to tell a first run from an existing install
// and logs the matching hint. A count failure is fatal.
func logFirstRunStatus(database *db.DB) {
	userCount, err := models.CountUsers(database.DB)
	if err != nil {
		slog.Error("failed to count users", "error", err)
		os.Exit(1)
	}

	if userCount == 0 {
		slog.Info("first run: open the admin URL shown above to create your account and run setup")
	} else {
		slog.Debug("existing installation detected", "users", userCount)
	}
}

// startBackgroundWorkers auto-starts the engine and job workers, starts the
// scheduler, maintenance, and bot services, and registers the settings-change
// subscribers that hot-reload bot providers and the daily token budget.
// Returns the non-fatal boot warnings collected for the dashboard and the bot
// service, which the admin API drives for setup and pairing.
func startBackgroundWorkers(engineCtx context.Context, cfg *config.Config, database *db.DB, projectDBMgr *db.ProjectDBManager, engineMgr *engine.Engine, jobsMgr *jobs.Manager, bus *events.Bus, encryptor *security.Encryptor, dailyTokenBudget *atomic.Int64) ([]string, *bot.BotService) {
	// Auto-start failures are non-fatal (the admin UI must stay reachable so
	// the operator can fix the cause), but a single boot log line is easy to
	// miss - collect them for the dashboard's system status too.
	var bootWarnings []string
	if err := engineMgr.AutoStart(); err != nil {
		slog.Error("engine auto-start failed", "error", err)
		bootWarnings = append(bootWarnings, fmt.Sprintf(msgcat.Mark("engine auto-start failed: %v (projects will not build until resolved)"), err))
	}
	if err := jobsMgr.AutoStart(); err != nil {
		slog.Error("jobs auto-start failed", "error", err)
		bootWarnings = append(bootWarnings, fmt.Sprintf(msgcat.Mark("jobs auto-start failed: %v (background jobs will not run until resolved)"), err))
	}

	engineMgr.StartScheduler()
	engineMgr.StartMaintenance()

	// Bot service (Telegram, etc.), provider-agnostic messaging.
	botService := startBotService(engineCtx, cfg, database, projectDBMgr, engineMgr, bus, encryptor)

	// Hot-reload bot providers when settings change (no restart needed).
	bus.Subscribe(events.EventSettingsChanged, func(e events.Event) {
		botService.ReloadTelegram(engineCtx)
	})

	// Re-resolve the engine daily token budget when settings change so an edit
	// from the admin UI or the setup wizard applies to the running engine
	// immediately (the next billable call reads the new ceiling).
	bus.Subscribe(events.EventSettingsChanged, func(e events.Event) {
		dailyTokenBudget.Store(int64(resolveEngineDailyTokenBudget(cfg, database)))
	})

	// Whole-instance backups on a schedule (off until the operator sets one). The
	// loop re-reads its cadence from settings each tick, so an edit from the admin
	// UI applies within a minute without a restart.
	go backupsched.New(database, backupsched.Config{DataDir: cfg.DataDir, DBPath: cfg.DBPath, Version: Version}, slog.Default()).Run(engineCtx)

	return bootWarnings, botService
}

// startTLSProxy constructs the TLS proxy manager, auto-starts it when a
// domain exists, gates secure cookies on whether TLS is actually bound, and
// emits the permissive-defaults warning for internet-reachable instances.
func startTLSProxy(cfg *config.Config, database *db.DB, bus *events.Bus, jwtMgr *security.JWTManager) *tlsproxy.Manager {
	tlsMgr := tlsproxy.NewManager(cfg, database.DB, slog.Default())
	tlsMgr.SubscribeToEvents(bus)
	if !cfg.TLSDisabled {
		// Auto-start only if at least one active project has a domain. With
		// no domains, the listeners stay idle - adding the first domain via
		// the admin UI fires an event that calls Start() live, no restart.
		if err := tlsMgr.StartIfDomainsExist(); err != nil {
			slog.Warn("TLS proxy could not start", "error", err)
			// Non-fatal: the admin UI on :AdminPort still works.
		}
	}
	// Now that we know whether TLS is bound, gate secure cookies on it.
	jwtMgr.SecureCookies = tlsMgr.IsRunning()

	// Nag once at startup if the instance is internet-reachable (has a public
	// domain) but the public router still serves wide-open defaults. Silent on a
	// local-only box, where those defaults are intentional.
	warnIfExposedWithPermissiveDefaults(cfg, tlsMgr)

	return tlsMgr
}

// networkHelpers are the background pieces behind the going-live flow: router
// port forwarding and the watch for a public IP that changed under a domain.
type networkHelpers struct {
	mapper  *portmap.Mapper
	watcher *netinfo.Watcher
}

// startNetworkHelpers starts the IP-change watch and, when the owner opted in
// earlier, re-creates the router port forwards. Both only ever talk to the
// router on the local network and to the normal DNS resolver.
func startNetworkHelpers(cfg *config.Config, database *db.DB) *networkHelpers {
	mapper := portmap.NewMapper(slog.Default())
	watcher := netinfo.NewWatcher(netinfo.NewDetector(), func() []string {
		return publicProjectDomains(database)
	}, slog.Default())
	watcher.Start()

	if on, _ := database.GetSetting(portmap.SettingKey); on == "on" && !cfg.TLSDisabled {
		go func() {
			if _, err := mapper.Enable(context.Background()); err != nil {
				slog.Warn("could not restore router port forwarding", "error", err)
			}
		}()
	}
	return &networkHelpers{mapper: mapper, watcher: watcher}
}

// publicProjectDomains lists the internet domains of published projects, the
// ones whose DNS records the IP-change watch compares.
func publicProjectDomains(database *db.DB) []string {
	projects, err := models.ListPublishedProjects(database.DB)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range projects {
		if p.Domain != nil && tlsproxy.DomainKind(*p.Domain) == tlsproxy.DomainKindPublic {
			out = append(out, *p.Domain)
		}
	}
	return out
}

// stop removes the router forwards and ends the watch.
func (n *networkHelpers) stop() {
	n.watcher.Stop()
	n.mapper.Stop()
}

// newServer loads the admin UI assets, constructs the HTTP server with its
// full dependency set, wires the WebSocket broadcaster into the action
// runner, logs readiness, and kicks off the browser auto-open when
// configured. An asset load failure exits the process.
func newServer(a *appDeps, engineMgr *engine.Engine, jobsMgr *jobs.Manager, chatHandler *chat.ChatHandler, tlsMgr *tlsproxy.Manager, netHelpers *networkHelpers, actionRunner *triggers.Runner, bootWarnings []string, botService *bot.BotService) *server.Server {
	adminSubFS, err := adminAssets()
	if err != nil {
		slog.Error("failed to create admin sub-filesystem", "error", err)
		os.Exit(1)
	}

	srv := server.New(&server.ServerDeps{
		Core: deps.Core{
			Config:             a.cfg,
			DB:                 a.database,
			ProjectDBManager:   a.projectDBMgr,
			JWTManager:         a.jwtMgr,
			Encryptor:          a.encryptor,
			Bus:                a.bus,
			Engine:             engineMgr,
			Jobs:               jobsMgr,
			ChatHandler:        chatHandler,
			LLMRegistry:        a.llmRegistry,
			ToolRegistry:       a.toolRegistry,
			LLMHTTPClient:      a.llmHTTPClient,
			DailyTokenBudgetFn: a.budgetFn,
			Logger:             slog.Default(),
			Version:            Version,
			TLSManager:         tlsMgr,
			FirewallManager:    firewall.New(),
			PortMapper:         netHelpers.mapper,
			NetWatcher:         netHelpers.watcher,
			BootWarnings:       bootWarnings,
			Bot:                botService,
		},
		AdminFS: adminSubFS,
	})

	// Wire the WebSocket broadcaster into the Actions Runner so ws_broadcast actions work.
	actionRunner.SetBroadcaster(srv.WSBroadcaster())

	slog.Debug("Open Fabrica ready",
		"admin", a.cfg.AdminPort,
		"public", a.cfg.PublicPort,
		"tls", tlsMgr.IsRunning(),
	)

	if a.cfg.AutoOpenBrowser {
		adminURL := fmt.Sprintf("http://localhost:%d", a.cfg.AdminPort)
		go openBrowserWhenReady(a.cfg.AdminPort, adminURL)
	}

	return srv
}

// collectRegistrationErrors drains and logs the init()-time registration errors
// from every self-registering subsystem (the tool registry plus each provider
// registry). Draining all of them, rather than letting the first be a panic,
// means a duplicate name or missing metadata surfaces alongside its siblings.
// A non-empty result means startup must abort.
func collectRegistrationErrors() []error {
	var errs []error
	drain := func(label string, src []error) {
		for _, e := range src {
			slog.Error(label, "error", e)
			errs = append(errs, e)
		}
	}
	drain("tool registration error", tools.RegistrationErrors())
	drain("llm provider registration error", llm.RegistrationErrors())
	drain("llm capability rule error", llm.ValidateModelCaps())
	drain("imagegen provider registration error", imagegen.RegistrationErrors())
	drain("stocksearch provider registration error", stocksearch.RegistrationErrors())
	drain("websearch provider registration error", websearch.RegistrationErrors())
	return errs
}

// wireEngineWakeSubscribers registers the event-bus subscribers that drive the
// per-project engine and job workers: auto-start on project creation, and wake
// the engine when a question is answered or the user sends a chat message. The
// wake semantics live here in one named place instead of inline in main.
func wireEngineWakeSubscribers(bus *events.Bus, engineMgr *engine.Engine, jobsMgr *jobs.Manager) {
	// Auto-start the engine when a new project is created (via any path).
	bus.Subscribe(events.EventProjectCreated, func(e events.Event) {
		if err := engineMgr.StartProject(e.ProjectID); err != nil {
			slog.Error("failed to auto-start engine on project creation", "project_id", e.ProjectID, "error", err)
		}
		if err := jobsMgr.StartProject(e.ProjectID); err != nil {
			slog.Error("failed to auto-start job worker on project creation", "project_id", e.ProjectID, "error", err)
		}
	})

	// Stop the job worker when a project is deleted (via any path: the admin
	// handler, the Telegram bot). StopProject is idempotent, so the admin path that
	// already stopped it before removing the data is unaffected; without this a
	// delete from the bot left the worker polling a closed database until restart.
	bus.Subscribe(events.EventProjectDeleted, func(e events.Event) {
		jobsMgr.StopProject(e.ProjectID)
		tokenrevoke.Forget(e.ProjectID) // its cached session revocations go with it
	})

	// Wake the engine when a question is answered so it can resume building.
	// Pass the answer context so the engine can acknowledge it.
	bus.Subscribe(events.EventQuestionAnswered, func(e events.Event) {
		cmd := engine.Command{
			Type: engine.CommandWake,
			Payload: map[string]any{
				"reason":      "question_answered",
				"question_id": e.Payload["question_id"],
				"answer":      e.Payload["answer"],
			},
		}
		if err := engineMgr.Send(e.ProjectID, cmd); err != nil {
			slog.Error("failed to wake engine after question answered, retrying", "project_id", e.ProjectID, "error", err)
			time.AfterFunc(3*time.Second, func() {
				if err2 := engineMgr.Send(e.ProjectID, cmd); err2 != nil {
					slog.Error("retry wake after question answered also failed", "project_id", e.ProjectID, "error", err2)
				}
			})
		}
	})

	// Wake the engine when the user sends a chat message so it can validate any
	// changes the chat may have made, or fix things during the live phase.
	bus.Subscribe(events.EventChatMessage, func(e events.Event) {
		role, _ := e.Payload["role"].(string)
		if role != "user" {
			return
		}
		content, _ := e.Payload["content"].(string)
		if err := engineMgr.Send(e.ProjectID, engine.Command{
			Type:    engine.CommandChat,
			Payload: map[string]any{"message": content},
		}); err != nil {
			slog.Debug("failed to wake engine on chat message", "project_id", e.ProjectID, "error", err)
		}
	})
}

// startBotService constructs the provider-agnostic bot service (Telegram,
// ...), registers the Telegram provider when a token is configured, and
// starts it on ctx. Returns the service so the caller can hot-reload providers
// on a settings change.
func startBotService(ctx context.Context, cfg *config.Config, database *db.DB, projectDBMgr *db.ProjectDBManager, engineMgr *engine.Engine, bus *events.Bus, encryptor *security.Encryptor) *bot.BotService {
	adminBaseURL, _ := database.GetSetting("admin_base_url")
	botService := bot.NewBotService(&bot.BotDeps{
		DB:               database,
		ProjectDBManager: projectDBMgr,
		Engine:           engineMgr,
		Bus:              bus,
		Encryptor:        encryptor,
		Logger:           slog.Default(),
		AdminBaseURL:     adminBaseURL,
		TLSDisabled:      cfg.TLSDisabled,
	})
	bot.UpgradeBotToken(database, encryptor)
	if token := bot.LoadBotToken(database, encryptor); token != "" {
		// botService doubles as the OffsetStore (persists the poll offset).
		tg := bot.NewTelegramProvider(token, botService.HandleIncoming, botService.HandleCallback, botService)
		botService.RegisterProvider(tg)
	}
	go botService.Start(ctx)
	return botService
}

// gracefulShutdown stops the background managers in order, bounded by a deadline
// so a stuck engine/LLM goroutine cannot wedge the process forever. After the
// deadline the caller falls through to its deferred database closes; in-flight
// writers then see "database is closed".
func gracefulShutdown(cancelActions context.CancelFunc, tlsMgr *tlsproxy.Manager, netHelpers *networkHelpers, engineMgr *engine.Engine, jobsMgr *jobs.Manager) {
	// Abort in-flight webhook deliveries and trigger actions first: they are
	// fire-and-forget side effects nothing waits on, and left running they would
	// keep dialing out (and writing to project DBs) while the workers drain.
	cancelActions()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	done := make(chan struct{})
	go func() {
		_ = tlsMgr.Stop()
		netHelpers.stop()
		engineMgr.StopAll()
		jobsMgr.StopAll()
		// Pooled headless browsers are rooted at Background so a probe's context
		// cannot kill a shared one, which also means nothing else reaps them.
		// After the workers stop, so a draining probe keeps its browser.
		store.ShutdownBrowserPool()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("Open Fabrica shutdown complete")
	case <-shutdownCtx.Done():
		slog.Warn("Open Fabrica shutdown timeout exceeded, forcing exit", "timeout", "30s")
	}
}
