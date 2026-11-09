package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/projectident"
	"openfabrica/internal/safego"
)

// pendingActionTTL bounds how long a multi-step flow (/new etc.) stays armed
// before we assume the user has wandered off and drop it. Answer flows are
// exempt: they persist until the underlying question is resolved.
const pendingActionTTL = 10 * time.Minute

// statusCoalesceWindow is how long a single status bubble can keep being edited
// before we post a fresh one. Keeps the chat from accumulating stale heads.
const statusCoalesceWindow = 5 * time.Minute

// projectsPerPage caps how many projects a single /projects keyboard shows;
// older installs can have many, so the list paginates.
const projectsPerPage = 8

// BotService orchestrates messaging providers and routes messages between
// bot users and the engine. It is provider-agnostic; Telegram,
// Slack etc. are pluggable via the Provider interface.
type BotService struct {
	deps      *BotDeps
	providers map[string]Provider
	users     map[string]*UserState // key: "provider:chatID"
	mu        sync.RWMutex
	logger    *slog.Logger

	// rl is the per-user command rate limiter (own lock; see ratelimit.go).
	rl *rateLimiter
	// usersMu serializes the allowlist's read-modify-write (add, remove): two
	// concurrent edits would otherwise read the same list and the later write
	// would drop the earlier one.
	usersMu sync.Mutex

	subOnce sync.Once
	// resendOnce re-sends the questions still waiting once per process, after the
	// first provider is up.
	resendOnce sync.Once

	// notifyQueue serialises the outbound event notifications (see queued).
	notifyOnce  sync.Once
	notifyQueue chan func()

	// live holds the per-project live status cards.
	live liveHub

	// pair holds invite-link pairing state (own lock; see pairing.go).
	pair *pairingStore

	// names holds display names (own lock + persistence; see namestore.go).
	names *nameStore

	// langs holds per-person UI language (own lock + persistence; see langstore.go).
	langs *langStore

	// sd is the self-destruct confirmation and its installed hooks.
	sd selfDestructState
}

// NewBotService creates a new bot service.
func NewBotService(deps *BotDeps) *BotService {
	return &BotService{
		deps:      deps,
		providers: make(map[string]Provider),
		users:     LoadSessions(deps.DB),
		rl:        newRateLimiter(),
		pair:      newPairingStore(),
		langs:     newLangStore(deps.DB),
		names:     newNameStore(deps.DB),
		logger:    deps.Logger.With("component", "bot"),
	}
}

// RegisterProvider adds a messaging provider.
func (s *BotService) RegisterProvider(p Provider) {
	if tp, ok := p.(*TelegramProvider); ok {
		tp.SetUserSeen(s.noteUser)
		tp.SetLanguageSeen(s.noteLanguage)
		tp.SetUnsupported(s.replyUnsupported)
	}
	s.mu.Lock()
	s.providers[p.Name()] = p
	s.mu.Unlock()
	s.logger.Info("registered bot provider", "provider", p.Name())
}

// provider returns the registered provider by name. Reads go through here so
// they never race a hot reload that swaps the provider map entry.
func (s *BotService) provider(name string) (Provider, bool) {
	s.mu.RLock()
	p, ok := s.providers[name]
	s.mu.RUnlock()
	return p, ok
}

// LoadOffset / SaveOffset implement OffsetStore, persisting the Telegram poll
// offset in the settings table so a restart resumes cleanly. SetSetting does not
// publish EventSettingsChanged, so this never triggers a provider reload.
func (s *BotService) LoadOffset() int {
	v, err := s.deps.DB.GetSetting("telegram_offset")
	if err != nil || v == "" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	if n < 0 {
		return 0
	}
	return n
}

func (s *BotService) SaveOffset(offset int) {
	if err := s.deps.DB.SetSetting("telegram_offset", strconv.Itoa(offset)); err != nil {
		// In-run this is harmless (the poller's in-memory offset still advances),
		// but a persistent failure means a restart replays from the last saved
		// offset, worth surfacing rather than swallowing.
		s.logger.Warn("failed to persist telegram offset", "error", err)
	}
}

// ReloadTelegram reconciles the running Telegram provider with the configured
// telegram_bot_token: starts it when a token appears, stops it when removed, and
// restarts it when the token value rotates. Safe to call at any time.
func (s *BotService) ReloadTelegram(ctx context.Context) {
	// A token added while the app is running must still receive engine events, so
	// make sure the bus subscriptions exist regardless of how Start went.
	s.subscribe()
	token := LoadBotToken(s.deps.DB, s.deps.Encryptor)

	s.mu.RLock()
	existing, hasExisting := s.providers["telegram"]
	s.mu.RUnlock()

	currentToken := ""
	if tp, ok := existing.(*TelegramProvider); ok {
		currentToken = tp.token
	}
	if token == currentToken {
		return // no change
	}

	if hasExisting {
		_ = existing.Stop() // blocks until the old poll loop exits (no double poller)
		s.mu.Lock()
		delete(s.providers, "telegram")
		s.mu.Unlock()
		if token == "" {
			resetBotStats()
			s.logger.Info("telegram bot stopped (token removed)")
			return
		}
		s.logger.Info("telegram bot restarting (token rotated)")
	}

	if token != "" {
		tg := NewTelegramProvider(token, s.HandleIncoming, s.HandleCallback, s)
		s.RegisterProvider(tg)
		safego.Go("bot-provider[telegram]", func() {
			if err := tg.Start(ctx); err != nil {
				s.logger.Error("bot provider stopped", "provider", tg.Name(), "error", err)
			}
		})
		s.logger.Info("telegram bot started", "rotated", hasExisting)
		s.scheduleResend()
	}
}

// subscribe registers the engine event handlers exactly once. It is called from
// Start and ReloadTelegram so the bot relays events even when the first provider
// only appears after boot.
func (s *BotService) subscribe() {
	s.subOnce.Do(func() {
		s.deps.Bus.Subscribe(events.EventEngineMessage, s.onBrainMessage)
		s.deps.Bus.Subscribe(events.EventQuestionAsked, s.onQuestionAsked)
		// These handlers send over the network. Bus.Publish runs handlers on the
		// publisher's goroutine, so inline they would stall the engine worker (or a
		// public webhook request) behind Telegram. Queued, they still go out in order.
		s.deps.Bus.Subscribe(events.EventEngineStageChange, s.queued(s.onStageChange))
		s.deps.Bus.Subscribe(events.EventProjectPhaseChanged, s.queued(s.onPhaseChanged))
		s.deps.Bus.Subscribe(events.EventEngineError, s.queued(s.onBrainError))
		s.deps.Bus.Subscribe(events.EventEngineStarted, s.queued(s.onBrainStarted))
		s.deps.Bus.Subscribe(events.EventEngineStopped, s.queued(s.onBrainStopped))
		s.deps.Bus.Subscribe(events.EventToolFailed, s.queued(s.onToolFailed))
		s.deps.Bus.Subscribe(events.EventWebhookReceived, s.queued(s.onWebhookReceived))
		s.deps.Bus.Subscribe(events.EventPaymentCompleted, s.queued(s.onPaymentCompleted))
		s.deps.Bus.Subscribe(events.EventPaymentFailed, s.queued(s.onPaymentFailed))

		// Live status card.
		s.deps.Bus.Subscribe(events.EventEngineThinking, s.onThinking)
		s.deps.Bus.Subscribe(events.EventEngineToolStart, s.onToolStart)
		s.deps.Bus.Subscribe(events.EventEngineToolResult, s.onToolResult)
		s.deps.Bus.Subscribe(events.EventEngineTasks, s.onTasks)
		s.deps.Bus.Subscribe(events.EventEngineLLMCall, s.onLLMCall)
		s.deps.Bus.Subscribe(events.EventEngineStopLanded, s.onStopLanded)
	})
}

// Start launches all registered providers and subscribes to bus events.
func (s *BotService) Start(ctx context.Context) {
	s.subscribe()

	s.mu.RLock()
	providers := make(map[string]Provider, len(s.providers))
	for name, p := range s.providers {
		providers[name] = p
	}
	s.mu.RUnlock()
	if len(providers) == 0 {
		s.logger.Info("no bot providers configured yet")
		return
	}

	for name, p := range providers {
		s.logger.Info("starting bot provider", "provider", name)
		p := p
		safego.Go("bot-provider["+name+"]", func() {
			if err := p.Start(ctx); err != nil {
				s.logger.Error("bot provider stopped", "provider", p.Name(), "error", err)
			}
		})
	}
	s.scheduleResend()
}

// isAllowed checks the telegram_allowed_users setting. If no users are
// configured, access is denied.
func (s *BotService) isAllowed(userID string) bool {
	if userID == "" {
		return false
	}
	allowed, err := s.deps.DB.GetSetting("telegram_allowed_users")
	if err != nil || allowed == "" {
		return false
	}
	for _, id := range strings.Split(allowed, ",") {
		if strings.TrimSpace(id) == userID {
			return true
		}
	}
	return false
}

// replyUnsupported answers a photo, voice note or file, which the builder cannot
// read yet, so the person is not left wondering why nothing happened.
func (s *BotService) replyUnsupported(chatID, userID string) {
	if !s.isAllowed(userID) || !s.allowRate("unsupported:"+chatID) {
		return
	}
	if p, ok := s.provider("telegram"); ok {
		_, _ = p.SendMessage(chatID, s.voiceForUser(userID).tr("I can only read text messages for now. Please type it out."))
	}
}

// allowRate applies a per-user token-bucket rate limit. Empty userID (unknown
// sender) is allowed through to the auth check, which will reject it.
func (s *BotService) allowRate(userID string) bool {
	return s.rl.allow(userID)
}

// HandleIncoming processes a message from any provider.
func (s *BotService) HandleIncoming(provider, chatID, userID string, messageID int, text string) {
	// An invite link ("/start <code>") is the one message a stranger may send.
	if s.tryPair(provider, chatID, userID, strings.TrimSpace(text)) {
		return
	}
	if !s.isAllowed(userID) {
		incAuthDenials()
		s.logger.Warn("bot auth denied", "provider", provider, "userID", userID)
		// Reply at most a few times a minute per chat so a stranger cannot make
		// the bot spam back.
		if !s.allowRate("deny:" + chatID) {
			return
		}
		if p, ok := s.provider(provider); ok {
			_, _ = p.SendMessage(chatID, s.voiceForUser(userID).trh("This bot is private. To get access, ask the owner for an invite link, or give them your user ID: %s", userID))
		}
		return
	}
	if !s.allowRate(userID) {
		if p, ok := s.provider(provider); ok {
			_, _ = p.SendMessage(chatID, s.voiceForUser(userID).tr("Slow down a moment, too many requests."))
		}
		return
	}

	state := s.getOrCreateUser(provider, chatID)
	state.Mu.Lock()
	defer state.Mu.Unlock()

	if userID != "" && state.UserID != userID {
		state.UserID = userID
		SaveSession(s.deps.DB, state)
	}

	text = strings.TrimSpace(text)

	// Expire stale pending flows (except question answers and in-progress forms,
	// which persist until the underlying question is resolved) so a delayed reply
	// doesn't trigger a stale action.
	if state.PendingAction != "" && state.PendingAction != "awaiting_answer" && state.PendingAction != "awaiting_form" &&
		!state.PendingActionAt.IsZero() && time.Since(state.PendingActionAt) > pendingActionTTL {
		state.PendingAction = ""
		state.PendingData = nil
	}

	// Commands always take precedence so /cancel, /help etc. escape a pending
	// flow instead of being swallowed as its input.
	if state.PendingAction != "" && !strings.HasPrefix(text, "/") {
		s.handlePending(state, text, messageID)
		return
	}

	if strings.HasPrefix(text, "/") {
		s.handleCommand(state, text)
		return
	}

	s.handleChat(state, text)
}

// HandleCallback processes inline button presses. messageID is the message the
// button belongs to, so navigation can edit it in place. Returns toast text.
func (s *BotService) HandleCallback(provider, chatID, userID string, messageID int, data string) string {
	if !s.isAllowed(userID) {
		incAuthDenials()
		return ""
	}
	if !s.allowRate(userID) {
		return s.voiceForUser(userID).tr("Slow down a moment.")
	}

	state := s.getOrCreateUser(provider, chatID)
	state.Mu.Lock()
	defer state.Mu.Unlock()

	if userID != "" && state.UserID != userID {
		state.UserID = userID
		SaveSession(s.deps.DB, state)
	}
	v := s.voice(state)

	// Tapping anything other than the prompt's own buttons abandons a half-finished
	// "new project" / "rename" step, so a later message is not taken as its answer.
	if data != "nav:new" && data != "nav:rename" && data != "new:go" {
		s.dropEntryFlow(state)
	}

	// A screen opened from the live card must not overwrite the card itself.
	if messageID != 0 && messageID == state.LiveCardMessageID && strings.HasPrefix(data, "nav:") {
		messageID = 0
	}

	switch {
	case strings.HasPrefix(data, "nav:"):
		return s.handleNav(state, messageID, strings.TrimPrefix(data, "nav:"))

	case data == "new:go":
		if state.PendingAction != "confirm_new" || state.PendingData["idea"] == "" {
			return v.tr("That offer expired. Send your idea again.")
		}
		idea := state.PendingData["idea"]
		s.clearPending(state)
		s.createProject(state, idea)
		return ""

	case strings.HasPrefix(data, "q:"):
		return s.handleQuestionNav(state, messageID, strings.TrimPrefix(data, "q:"))

	case strings.HasPrefix(data, "card:"):
		return s.handleCardAction(state, strings.TrimPrefix(data, "card:"))

	case strings.HasPrefix(data, "log:"):
		return s.handleLog(state, messageID, strings.TrimPrefix(data, "log:"))

	case strings.HasPrefix(data, "verb:"):
		state.setVerbosity(strings.TrimPrefix(data, "verb:"))
		SaveSession(s.deps.DB, state)
		s.showScreen(state, messageID, s.alertsScreen(state))
		return v.tr("Saved")

	case strings.HasPrefix(data, "projpage:"):
		n := 0
		_, _ = fmt.Sscanf(strings.TrimPrefix(data, "projpage:"), "%d", &n)
		if n < 0 {
			n = 0
		}
		s.showScreen(state, messageID, s.projectsScreen(state, n))
		return ""

	case strings.HasPrefix(data, "project:"):
		var projectID int
		_, _ = fmt.Sscanf(strings.TrimPrefix(data, "project:"), "%d", &projectID)
		if projectID > 0 {
			name := s.switchProject(state, projectID)
			// Land the user on the project's chat screen (the picker message
			// becomes the chat header).
			s.showScreen(state, messageID, s.projectChatScreen(state))
			if name != "" {
				return v.tr("Opened %s", name)
			}
		}
		return ""

	case strings.HasPrefix(data, "phase:"):
		mode := strings.TrimPrefix(data, "phase:")
		if err := s.applyMode(state, mode); err != nil {
			return v.tr("Failed")
		}
		// Re-render settings so the new phase is reflected in place.
		s.showScreen(state, messageID, s.settingsScreen(state))
		return v.tr("Phase: %s", v.modeLabel(mode))

	case strings.HasPrefix(data, "act:"):
		return s.handleAction(state, strings.TrimPrefix(data, "act:"))

	case strings.HasPrefix(data, "cfm:"):
		return s.handleConfirm(state, messageID, strings.TrimPrefix(data, "cfm:"))

	// a:{questionID}:{optIndex} -> answer using the indexed option. Telegram
	// caps callback_data at 64 bytes, so we encode by option index, not text.
	case strings.HasPrefix(data, "a:"):
		parts := strings.SplitN(data, ":", 3)
		if len(parts) == 3 {
			s.answerQuestionByIndex(state, parts[1], parts[2])
			return v.tr("Answer recorded")
		}

	// m:{questionID}:{optIndex|done} -> toggle one option of a multi-choice
	// question, or submit the selection.
	case strings.HasPrefix(data, "m:"):
		parts := strings.SplitN(data, ":", 3)
		if len(parts) == 3 {
			return s.toggleMultiChoice(state, messageID, parts[1], parts[2])
		}
	}

	return ""
}

// handleAction runs a one-tap quick action (offered after /status).
func (s *BotService) handleAction(state *UserState, action string) string {
	v := s.voice(state)
	switch action {
	case "pause":
		s.setMode(state, models.ModePaused)
		return v.tr("Pausing")
	case "resume":
		s.setMode(state, s.resumeMode(state.ActiveProjectID))
		return v.tr("Resuming")
	case "wake":
		s.cmdWake(state)
		return v.tr("Waking")
	}
	return ""
}

// handleConfirm resolves a confirmation prompt. messageID lets it re-render the
// originating screen in place after the action.
func (s *BotService) handleConfirm(state *UserState, messageID int, action string) string {
	// Confirmations that destroy something carry the project they were shown for
	// ("delete:12"). One that no longer matches the active project, or an old
	// button with no project at all, is refused instead of acting on a guess.
	action, boundTo, _ := strings.Cut(action, ":")
	v := s.voice(state)
	if action == "delete" {
		if pid, err := strconv.Atoi(boundTo); err != nil || pid != state.ActiveProjectID {
			s.send(state, v.tr("That confirmation is out of date, so nothing was deleted. Open Settings and choose Delete again."))
			return v.tr("Expired")
		}
	}
	switch action {
	case "sd":
		// boundTo carries the one-time token, not a project id.
		return s.confirmSelfDestruct(state, messageID, boundTo)
	case "cancel":
		s.send(state, v.tr("Cancelled."))
		return v.tr("Cancelled")
	case "stop":
		s.doStop(state)
		return v.tr("Stopping")
	case "delete":
		if err := s.deleteProject(state); err != nil {
			s.send(state, v.trh("Delete failed: %s", err))
			return v.tr("Delete failed")
		}
		s.showScreen(state, messageID, s.mainMenuScreen(v))
		return v.tr("Deleted")
	}
	return ""
}

// --- Command handlers ---

func (s *BotService) handleCommand(state *UserState, text string) {
	cmd := strings.ToLower(strings.SplitN(text, " ", 2)[0])
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}

	s.logger.Debug("bot command", "cmd", cmd, "userID", state.UserID, "project", state.ActiveProjectID)
	v := s.voice(state)

	if cmd != "/cancel" {
		s.dropEntryFlow(state)
	}

	switch cmd {
	case "/start", "/menu":
		s.showScreen(state, 0, s.dashboardScreen(state))

	case "/status", "/cost":
		s.openNav(state, "status")
	case "/questions":
		s.openNav(state, "questions")
	case "/log":
		s.openNav(state, "log")
	case "/site":
		s.openNav(state, "site")
	case "/projects":
		s.openNav(state, "projects")
	case "/alerts":
		s.openNav(state, "alerts")
	case "/help":
		s.openNav(state, "help")
	case "/selfdestruct":
		s.openNav(state, "selfdestruct")
	case "/pause", "/resume", "/stop":
		if toast := s.handleCardAction(state, strings.TrimPrefix(cmd, "/")); toast != "" {
			s.send(state, escapeHTML(toast))
		}

	case "/cancel":
		if state.PendingAction != "" {
			state.PendingAction = ""
			state.PendingData = nil
			state.PendingActionAt = time.Time{}
			SaveSession(s.deps.DB, state)
			s.send(state, v.tr("Cancelled."))
		} else {
			s.send(state, v.tr("Nothing to cancel."))
		}

	default:
		s.send(state, v.tr("Send /menu to open the menu."))
	}
}

// openNav shows a screen as a fresh message, the way a slash command does.
func (s *BotService) openNav(state *UserState, screenName string) {
	if toast := s.handleNav(state, 0, screenName); toast != "" {
		s.send(state, escapeHTML(toast))
	}
}

// --- Multi-step flow handlers ---

func (s *BotService) handlePending(state *UserState, text string, msgID int) {
	action := state.PendingAction
	if action == "awaiting_answer" {
		// Answer the question the user was shown, not whichever is oldest.
		shown := state.PendingData["question_id"]
		state.PendingAction = ""
		state.PendingActionAt = time.Time{}
		SaveSession(s.deps.DB, state)
		s.answerPendingQuestion(state, shown, text, msgID)
		return
	}
	// The form flow manages its own multi-step state, so hand off before the
	// generic clear below wipes PendingData.
	if action == "awaiting_form" {
		s.handleFormReply(state, text, msgID)
		return
	}

	state.PendingAction = ""
	state.PendingData = nil
	state.PendingActionAt = time.Time{}
	SaveSession(s.deps.DB, state)
	switch action {
	case "awaiting_description":
		s.createProject(state, text)
	case "confirm_new":
		s.offerProjectForMessage(state, text) // a newer message replaces the idea
	case "awaiting_rename":
		s.renameProject(state, text)
	default:
		s.send(state, s.voice(state).tr("Cancelled. Send /menu to start over."))
	}
}

// --- Chat (free text -> engine) ---

func (s *BotService) handleChat(state *UserState, text string) {
	if state.ActiveProjectID == 0 {
		s.offerProjectForMessage(state, text)
		return
	}
	v := s.voice(state)

	if s.deps.Engine == nil {
		s.send(state, v.tr("The builder isn't available right now."))
		return
	}

	// Immediate "typing" so the user knows the message landed even though the
	// engine's reply may take a while.
	if p, ok := s.provider(state.Provider); ok {
		_ = p.SendChatAction(state.ChatID, "typing")
	}

	if !s.deps.Engine.IsRunning(state.ActiveProjectID) {
		s.send(state, v.tr("Open Fabrica is not running, starting it now..."))
		if err := s.deps.Engine.StartProject(state.ActiveProjectID); err != nil {
			s.send(state, v.trh("Open Fabrica couldn't be started: %s", err))
			return
		}
	}

	cmd := engine.Command{
		Type:    engine.CommandChat,
		Payload: map[string]any{"message": text},
	}
	if err := s.deps.Engine.Send(state.ActiveProjectID, cmd); err != nil {
		s.send(state, v.trh("Failed to send message: %s", err))
		return
	}
	// The card now sits above this exchange; show the message landed right away.
	state.LiveCardStale.Store(true)
	s.cardUpdate(state.ActiveProjectID, func(st *cardState) {
		st.thinking = true
		st.stopped = false
	})
}

// --- Project operations ---

func (s *BotService) createProject(state *UserState, idea string) {
	idea = strings.TrimSpace(idea)
	v := s.voice(state)
	defModel, _, err := models.GetDefaultModel(s.deps.DB.DB)
	if err != nil {
		s.send(state, v.tr("No default model configured. Please set one in the admin panel first."))
		return
	}

	// Same identity the admin gives a project created without a name: a friendly
	// name plus a domain, with the name flagged so the engine can retitle it from
	// the idea.
	name, domain, err := projectident.Generate(s.deps.DB.DB)
	if err != nil {
		s.send(state, v.trh("Failed to create project: %s", err))
		return
	}
	project, err := models.CreateProject(s.deps.DB.DB, name, &domain, &idea, defModel.ID, nil, engine.DefaultAmbition)
	if err != nil {
		s.send(state, v.trh("Failed to create project: %s", err))
		return
	}
	if err := models.SetProjectNameAutoGenerated(s.deps.DB.DB, project.ID, true); err != nil {
		s.logger.Warn("failed to flag project name as auto-generated", "project", project.ID, "error", err)
	}

	if _, err := s.deps.ProjectDBManager.Create(project.ID); err != nil {
		if derr := models.DeleteProject(s.deps.DB.DB, project.ID); derr != nil {
			s.logger.Error("cleanup of half-created project failed", "project", project.ID, "error", derr)
		}
		s.send(state, v.trh("Failed to create project database: %s", err))
		return
	}

	s.deps.Bus.Publish(events.NewEvent(events.EventProjectCreated, project.ID, map[string]any{
		"name": project.Name,
	}))

	s.switchProject(state, project.ID)
	s.send(state, v.trh("🚀 <b>%s</b> is starting. I'm getting to know your idea, and the card below will keep you posted.", project.Name))
	// The idea is the project brief, so the builder can begin as soon as it runs.
	if s.deps.Engine != nil {
		if err := s.deps.Engine.StartProject(project.ID); err != nil {
			s.send(state, v.trh("The builder couldn't be started: %s", err))
		}
	}
	s.cardUpdate(project.ID, func(st *cardState) { st.thinking = true })
	state.LiveCardStale.Store(true)
	s.showScreen(state, 0, s.dashboardScreen(state))
}

// switchProject sets the active project (resetting the coalesced status bubble
// so the next status message starts fresh) and returns the project name, or ""
// if the project can't be loaded. It does not send a message: callers render the
// appropriate screen (chat header) themselves.
func (s *BotService) switchProject(state *UserState, projectID int) string {
	state.ActiveProjectID = projectID
	state.LastStatusMessageID = 0
	state.LastStatusText = ""
	SaveSession(s.deps.DB, state)

	project, err := models.GetProjectByID(s.deps.DB.DB, projectID)
	if err != nil {
		return ""
	}
	return project.Name
}

// dropEntryFlow abandons the short "type a description / type a name" prompts.
// Question and form answers are left alone: they persist until resolved.
func (s *BotService) dropEntryFlow(state *UserState) {
	switch state.PendingAction {
	case "awaiting_description", "awaiting_rename", "confirm_new":
		s.clearPending(state)
	}
}

// clearPending resets any in-progress multi-step flow.
func (s *BotService) clearPending(state *UserState) {
	state.PendingAction = ""
	state.PendingData = nil
	state.PendingActionAt = time.Time{}
	SaveSession(s.deps.DB, state)
}

func (s *BotService) pendingQuestionCount(pdb *db.ProjectDB) int {
	var n int
	_ = pdb.QueryRow("SELECT COUNT(*) FROM of_questions WHERE status = 'pending'").Scan(&n)
	return n
}

// --- Helpers ---

func (s *BotService) getOrCreateUser(provider, chatID string) *UserState {
	key := provider + ":" + chatID
	s.mu.Lock()
	defer s.mu.Unlock()

	if state, ok := s.users[key]; ok {
		return state
	}
	state := &UserState{Provider: provider, ChatID: chatID}
	s.users[key] = state
	return state
}

func (s *BotService) send(state *UserState, text string) {
	if p, ok := s.provider(state.Provider); ok {
		if _, err := p.SendMessage(state.ChatID, text); err != nil {
			s.logger.Warn("failed to send bot message", "provider", state.Provider, "chatID", state.ChatID, "error", err)
		}
	}
}

// broadcastToProjectUsers sends a message to all users on a project (always
// delivered, ignores mute). Targets are snapshotted under the lock and the HTTP
// sends happen outside it.
func (s *BotService) broadcastToProjectUsers(projectID int, text string) {
	s.broadcastRender(projectID, plainRender(text))
}

// broadcastRender is broadcastToProjectUsers for a message that is worded per
// person: render is called once for each recipient with their voice.
func (s *BotService) broadcastRender(projectID int, render func(voice) string) {
	s.mu.RLock()
	var targets []*UserState
	for _, state := range s.users {
		if state.ActiveProjectID == projectID {
			targets = append(targets, state)
		}
	}
	s.mu.RUnlock()

	for _, state := range targets {
		if p, ok := s.provider(state.Provider); ok {
			if _, err := p.SendMessage(state.ChatID, render(s.voice(state))); err != nil {
				s.logger.Warn("broadcast send failed", "error", err, "chatID", state.ChatID)
			}
			state.LiveCardStale.Store(true)
		}
	}
}

// broadcastStatus delivers a coalesce-eligible status notification to everyone
// on the project, editing their most recent status bubble when one exists within
// the window. Respects per-user mute.
func (s *BotService) broadcastStatus(projectID int, render func(voice) string) {
	s.broadcastStatusTo(projectID, render, func(*UserState) bool { return true })
}

// broadcastMilestone is broadcastStatus for lifecycle changes (stage, mode,
// start, stop). People with the live card already see these on it, so only
// quiet people get the separate line.
func (s *BotService) broadcastMilestone(projectID int, render func(voice) string) {
	s.broadcastStatusTo(projectID, render, func(st *UserState) bool { return st.verbosity() == VerbosityQuiet })
}

func (s *BotService) broadcastStatusTo(projectID int, render func(voice) string, include func(*UserState) bool) {
	s.mu.RLock()
	states := make([]*UserState, 0)
	for _, state := range s.users {
		if state.ActiveProjectID == projectID && !state.Muted && include(state) {
			states = append(states, state)
		}
	}
	s.mu.RUnlock()

	for _, state := range states {
		p, ok := s.provider(state.Provider)
		if !ok {
			continue
		}
		text := render(s.voice(state))
		state.Mu.Lock()
		canEdit := state.LastStatusMessageID != 0 &&
			!state.LastStatusAt.IsZero() &&
			time.Since(state.LastStatusAt) < statusCoalesceWindow &&
			state.LastStatusText != text
		if canEdit {
			err := p.EditMessage(state.ChatID, state.LastStatusMessageID, text)
			if err == nil {
				state.LastStatusText = text
				state.LastStatusAt = time.Now()
				state.Mu.Unlock()
				SaveSession(s.deps.DB, state)
				continue
			}
			// Edit can fail if the message is gone or unchanged; fall through to a fresh send.
			s.logger.Debug("status edit failed, posting fresh", "error", err)
		}
		state.Mu.Unlock()
		id, err := p.SendMessage(state.ChatID, text)
		if err != nil {
			s.logger.Warn("status send failed", "error", err)
			continue
		}
		state.Mu.Lock()
		state.LastStatusMessageID = id
		state.LastStatusAt = time.Now()
		state.LastStatusText = text
		state.Mu.Unlock()
		SaveSession(s.deps.DB, state)
	}
}

// parseOptionsJSON decodes a question's stored options ("[]"/empty -> nil). It
// takes a JSON array of strings or of {"label"} objects, and falls back to a
// comma-separated list the way the admin does (approval questions store
// "Approve, Deny").
func parseOptionsJSON(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" || s == "null" {
		return nil
	}
	var raw []any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		var out []string
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	var out []string
	for _, v := range raw {
		switch t := v.(type) {
		case string:
			if t != "" {
				out = append(out, t)
			}
		case map[string]any:
			label, _ := t["label"].(string)
			if label == "" {
				label, _ = t["value"].(string)
			}
			if label != "" {
				out = append(out, label)
			}
		}
	}
	return out
}
