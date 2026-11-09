package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/projectphase"
	"openfabrica/internal/safego"
)

// The live card is one message per person that edits itself while the engine
// works: stage, task progress, the current step, and the latest LLM call. Engine
// events only update an in-memory snapshot; a per-project pump renders it at
// most every cardMinInterval, so a burst of events costs one edit.

const cardMaxSteps = 4

// Timings, variables so tests can shrink them.
var (
	// Fastest a card is edited: Telegram allows about one edit a second per chat.
	cardMinInterval = 2 * time.Second
	// A card goes quiet this long after the last event (unless the model is
	// mid-call), so an idle project costs nothing.
	cardIdleAfter = 20 * time.Second
	// Longest a "thinking" flag is trusted without a fresh event.
	cardThinkingMax = 5 * time.Minute
	// A card that other messages have pushed up the chat is reposted at the
	// bottom, but not more often than this.
	cardRepostEvery = 15 * time.Second
	// How often a quiet pump wakes to check for idleness and refresh the
	// typing indicator.
	cardTick = 4 * time.Second
)

// Verbosity levels.
const (
	VerbosityQuiet   = "quiet"
	VerbosityNormal  = "normal"
	VerbosityVerbose = "verbose"
)

func normalizeVerbosity(v string) string {
	switch v {
	case VerbosityQuiet, VerbosityVerbose:
		return v
	}
	return VerbosityNormal
}

type cardStep struct {
	callID string
	// tool and action are what the label is made of, kept apart so each reader
	// gets the label in their own language. A step with no tool shows label as is.
	tool, action string
	label        string
	status       string // run, ok, fail, cancelled
}

type cardLLM struct {
	model     string
	in, out   int
	durMs     int64
	cost      float64
	costKnown bool
	failed    bool
	cacheHit  bool
}

// cardState is the snapshot the card renders from.
type cardState struct {
	stage       string
	paused      bool // read from the project phase when the card renders
	thinking    bool
	stopped     bool
	total, done int
	activeTask  string
	steps       []cardStep
	last        *cardLLM

	calls     int
	inTokens  int
	outTokens int
	cost      float64
	costKnown bool
	runStart  time.Time

	lastEvent time.Time
	seq       uint64
	// idleShown is set once the pump has rendered the idle version; the next
	// event starts a fresh run (counters reset).
	idleShown bool
}

type projectCard struct {
	pid     int
	mu      sync.Mutex
	st      cardState
	running bool
	wake    chan struct{}
}

type liveHub struct {
	mu    sync.Mutex
	cards map[int]*projectCard
}

func (h *liveHub) card(pid int) *projectCard {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cards == nil {
		h.cards = make(map[int]*projectCard)
	}
	c := h.cards[pid]
	if c == nil {
		c = &projectCard{pid: pid, wake: make(chan struct{}, 1)}
		h.cards[pid] = c
	}
	return c
}

// cardUpdate applies a change to a project's snapshot and makes sure the pump is
// running. It never blocks, so event handlers can call it inline.
func (s *BotService) cardUpdate(pid int, fn func(st *cardState)) {
	c := s.live.card(pid)
	c.mu.Lock()
	if c.st.idleShown {
		// New activity after an idle card: start a fresh run, keep what the engine
		// told us about where the project is.
		c.st = cardState{stage: c.st.stage, total: c.st.total, done: c.st.done}
	}
	if c.st.runStart.IsZero() {
		c.st.runStart = time.Now()
	}
	fn(&c.st)
	c.st.lastEvent = time.Now()
	c.st.seq++
	start := !c.running
	c.running = true
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
	if start {
		safego.Go("bot-livecard", func() { s.pump(c) })
	}
}

// --- event handlers ---

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func num(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

func (s *BotService) onThinking(e events.Event) {
	on := str(e.Payload, "state") == "start"
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		st.thinking = on
		if on {
			st.stopped = false
		}
	})
}

func (s *BotService) onToolStart(e events.Event) {
	tool, args := str(e.Payload, "tool"), e.Payload["args"]
	label := toolLabel(tool, args)
	action := toolAction(args)
	id := str(e.Payload, "call_id")
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		st.stopped = false
		st.steps = append(st.steps, cardStep{callID: id, tool: tool, action: action, label: label, status: "run"})
		if len(st.steps) > cardMaxSteps {
			st.steps = st.steps[len(st.steps)-cardMaxSteps:]
		}
	})
}

func (s *BotService) onToolResult(e events.Event) {
	id := str(e.Payload, "call_id")
	failed := str(e.Payload, "error") != ""
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		for i := range st.steps {
			if st.steps[i].callID == id {
				st.steps[i].status = map[bool]string{true: "fail", false: "ok"}[failed]
			}
		}
	})
}

func (s *BotService) onTasks(e events.Event) {
	total := int(num(e.Payload, "total"))
	done := int(num(e.Payload, "done"))
	active := ""
	switch rows := e.Payload["tasks"].(type) {
	case []map[string]any:
		for _, r := range rows {
			if str(r, "status") == engine.TaskActive {
				active = str(r, "title")
				break
			}
		}
	case []any:
		for _, item := range rows {
			if r, ok := item.(map[string]any); ok && str(r, "status") == engine.TaskActive {
				active = str(r, "title")
				break
			}
		}
	}
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		st.total, st.done, st.activeTask = total, done, active
	})
}

func (s *BotService) onLLMCall(e events.Event) {
	call := &cardLLM{
		model:    str(e.Payload, "model"),
		in:       int(num(e.Payload, "input_tokens")),
		out:      int(num(e.Payload, "output_tokens")),
		durMs:    int64(num(e.Payload, "duration_ms")),
		failed:   str(e.Payload, "error") != "",
		cacheHit: e.Payload["cache_hit"] == true,
	}
	if v, ok := e.Payload["cost_usd"].(float64); ok {
		call.cost, call.costKnown = v, true
	}
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		st.last = call
		st.calls++
		st.inTokens += call.in
		st.outTokens += call.out
		if call.costKnown {
			st.cost += call.cost
			st.costKnown = true
		}
	})
	s.postVerboseCall(e.ProjectID, call, e.Payload)
}

func (s *BotService) onStopLanded(e events.Event) {
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		st.stopped = true
		st.thinking = false
		for i := range st.steps {
			if st.steps[i].status == "run" {
				st.steps[i].status = "cancelled"
			}
		}
	})
}

// --- the pump ---

func (s *BotService) pump(c *projectCard) {
	var lastRender time.Time
	var lastTyping time.Time
	for {
		select {
		case <-c.wake:
		case <-time.After(cardTick):
		}
		if wait := cardMinInterval - time.Since(lastRender); wait > 0 {
			time.Sleep(wait)
		}

		c.mu.Lock()
		snap := c.st
		snap.steps = append([]cardStep(nil), c.st.steps...)
		if c.st.last != nil {
			l := *c.st.last
			snap.last = &l
		}
		idle := time.Since(snap.lastEvent) > cardIdleAfter && (!snap.thinking || time.Since(snap.lastEvent) > cardThinkingMax)
		if idle {
			snap.thinking = false
		}
		c.mu.Unlock()

		s.renderCard(c.pid, snap, idle)
		lastRender = time.Now()

		if snap.thinking && time.Since(lastTyping) > 4*time.Second {
			s.typingForProject(c.pid)
			lastTyping = time.Now()
		}

		if idle {
			c.mu.Lock()
			if c.st.seq == snap.seq {
				c.st.idleShown = true
				c.running = false
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
		}
	}
}

// cardTargets lists the people who should see a live card for a project.
func (s *BotService) cardTargets(pid int) []*UserState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*UserState
	for _, st := range s.users {
		if st.ActiveProjectID == pid && st.verbosity() != VerbosityQuiet && !st.Muted {
			out = append(out, st)
		}
	}
	return out
}

func (s *BotService) typingForProject(pid int) {
	for _, st := range s.cardTargets(pid) {
		if p, ok := s.provider(st.Provider); ok {
			_ = p.SendChatAction(st.ChatID, "typing")
		}
	}
}

func (s *BotService) renderCard(pid int, snap cardState, idle bool) {
	targets := s.cardTargets(pid)
	if len(targets) == 0 {
		return
	}
	name := ""
	if p, err := models.GetProjectByID(s.deps.DB.DB, pid); err == nil {
		name = p.Name
		snap.paused = s.projectPhase(p).Key == projectphase.KeyPaused
	}
	// One card per language: people watching the same project can read different
	// ones, so the body is worded once for each.
	byLang := map[string][]*UserState{}
	for _, st := range targets {
		lang := s.lang(st)
		byLang[lang] = append(byLang[lang], st)
	}
	for lang, group := range byLang {
		v := voice{lang: lang}
		cardName := name
		if cardName == "" {
			cardName = v.tr("Project")
		}
		text, labels, cbs := s.cardView(v, cardName, snap, idle)
		kb, err := buildInlineKeyboard(labels, cbs)
		if err != nil {
			continue
		}
		for _, st := range group {
			s.deliverCard(st, text, kb)
		}
	}
}

// deliverCard edits the person's card in place, or (re)posts it at the bottom of
// the chat when it is missing or has been pushed up by other messages.
func (s *BotService) deliverCard(st *UserState, text string, kb map[string]any) {
	p, ok := s.provider(st.Provider)
	if !ok {
		return
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()

	stale := st.LiveCardStale.Load() && time.Since(st.LiveCardAt) >= cardRepostEvery
	if st.LiveCardMessageID != 0 && !stale {
		if st.LiveCardText == text {
			return
		}
		err := p.EditMessageKeyboard(st.ChatID, st.LiveCardMessageID, text, kb)
		if err == nil || isNotModifiedErr(err) {
			st.LiveCardText = text
			return
		}
		// The message is gone or too old: fall through and post a new one.
	}
	if st.LiveCardMessageID != 0 {
		_ = p.DeleteMessage(st.ChatID, st.LiveCardMessageID)
	}
	id, err := p.SendCard(st.ChatID, text, kb, true)
	if err != nil {
		s.logger.Warn("live card send failed", "error", err)
		st.LiveCardMessageID = 0
		return
	}
	st.LiveCardMessageID = id
	st.LiveCardText = text
	st.LiveCardAt = time.Now()
	st.LiveCardStale.Store(false)
	SaveSession(s.deps.DB, st)
}

// --- rendering ---

func (v voice) stageName(stage string) string {
	switch strings.ToUpper(stage) {
	case "EXPAND":
		return v.tr("Getting to know your idea")
	case "PLAN":
		return v.tr("Mapping it out")
	case "BUILD":
		return v.tr("Building")
	case "LIVE":
		return v.tr("Live")
	}
	if stage == "" {
		return v.tr("Ready")
	}
	return stage
}

func progressBar(done, total int) string {
	const cells = 10
	filled := 0
	if total > 0 {
		filled = done * cells / total
		if filled == 0 && done > 0 {
			filled = 1
		}
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", cells-filled)
}

func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func fmtCost(usd float64) string {
	if usd < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// cardView renders the card body and its buttons.
func (s *BotService) cardView(v voice, name string, st cardState, idle bool) (string, []string, []string) {
	var b strings.Builder

	head := "🔨"
	state := ""
	switch {
	case st.paused:
		head, state = "⏸", v.tr("Paused")
	case st.stopped:
		head, state = "⏹", v.tr("Stopped")
	case st.thinking:
		head, state = "🧠", v.tr("Thinking")
	case idle:
		head, state = "💤", v.tr("Waiting for you")
	default:
		state = v.tr("Working")
	}
	fmt.Fprintf(&b, "%s <b>%s</b>\n%s", head, escapeHTML(name), escapeHTML(v.stageName(st.stage)))
	if state != "" {
		fmt.Fprintf(&b, " · %s", state)
	}
	b.WriteString("\n")

	if st.total > 0 {
		b.WriteString("\n" + v.tr("%s %d/%d tasks", progressBar(st.done, st.total), st.done, st.total) + "\n")
		if st.activeTask != "" {
			fmt.Fprintf(&b, "▶ %s\n", escapeHTML(truncate(st.activeTask, 90)))
		}
	}

	if len(st.steps) > 0 {
		b.WriteString("\n")
		for _, step := range st.steps {
			mark := map[string]string{"run": "⚙", "ok": "✓", "fail": "✗", "cancelled": "–"}[step.status]
			fmt.Fprintf(&b, "%s %s\n", mark, escapeHTML(v.stepLabel(step)))
		}
	}

	if st.last != nil {
		l := st.last
		line := fmt.Sprintf("%s · %s · %.1fs", truncate(l.model, 28), v.tr("%s in / %s out", fmtTokens(l.in), fmtTokens(l.out)), float64(l.durMs)/1000)
		if l.cacheHit {
			line += " · " + v.tr("replayed")
		}
		if l.failed {
			line += " · " + v.tr("failed")
		}
		if l.costKnown && !l.cacheHit {
			line += " · " + fmtCost(l.cost)
		}
		fmt.Fprintf(&b, "\n🧠 %s\n", escapeHTML(line))
	}
	if st.calls > 0 {
		total := v.tr("Σ %d calls · %s tokens", st.calls, fmtTokens(st.inTokens+st.outTokens))
		if st.costKnown {
			total += " · " + fmtCost(st.cost)
		}
		if !st.runStart.IsZero() {
			total += " · " + fmtDur(time.Since(st.runStart))
		}
		fmt.Fprintf(&b, "%s\n", escapeHTML(total))
	}

	labels := []string{v.tr("⏹ Stop"), v.tr("⏸ Pause"), v.tr("📜 LLM log"), v.tr("☰ Menu")}
	cbs := []string{"card:stop", "card:pause", "nav:log", "nav:menu"}
	if st.paused {
		labels[1], cbs[1] = v.tr("▶ Resume"), "card:resume"
	}
	if idle || st.stopped {
		labels, cbs = labels[1:], cbs[1:]
	}
	return b.String(), labels, cbs
}

// truncateRunes cuts s to at most n characters without splitting a multi-byte one.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// --- tool labels ---

var toolLabels = map[string]string{
	"manage_files": msgcat.Mark("Managing files"), "manage_media": msgcat.Mark("Processing image"),
	"manage_stockphotos": msgcat.Mark("Fetching stock photos"), "manage_web_search": msgcat.Mark("Searching the web"),
	"manage_data": msgcat.Mark("Managing data"), "manage_api": msgcat.Mark("Managing API endpoints"),
	"manage_auth": msgcat.Mark("Managing auth"), "manage_agent": msgcat.Mark("Managing agent"),
	"manage_chat": msgcat.Mark("Owner chat"), "manage_memory": msgcat.Mark("Managing memory"),
	"manage_notifications": msgcat.Mark("Managing notifications"), "manage_analytics": msgcat.Mark("Checking analytics"),
	"manage_diagnostics": msgcat.Mark("Running diagnostics"), "manage_testing": msgcat.Mark("Running active probe"),
	"manage_secrets": msgcat.Mark("Managing secrets"), "manage_providers": msgcat.Mark("Managing providers"),
	"manage_payments": msgcat.Mark("Managing payments"), "manage_email": msgcat.Mark("Managing email"),
	"manage_webhooks": msgcat.Mark("Managing webhooks"), "manage_http": msgcat.Mark("Making HTTP request"),
	"manage_project": msgcat.Mark("Managing project"), "manage_schedules": msgcat.Mark("Managing jobs"),
	"manage_schema": msgcat.Mark("Managing schema"), "manage_sql": msgcat.Mark("Running SQL"),
	"manage_uploads": msgcat.Mark("Managing uploads"), "manage_seo": msgcat.Mark("Managing SEO"),
	"manage_triggers": msgcat.Mark("Managing triggers"), "manage_libraries": msgcat.Mark("Managing libraries"),
	"manage_tasks": msgcat.Mark("Managing tasks"), "manage_function": msgcat.Mark("Managing functions"),
	"manage_codebase": msgcat.Mark("Searching codebase"), "manage_guides": msgcat.Mark("Reading guides"),
	"manage_knowledge": msgcat.Mark("Managing knowledge"), "manage_documents": msgcat.Mark("Managing documents"),
	"manage_image_gen": msgcat.Mark("Generating image"), "manage_redirects": msgcat.Mark("Managing redirects"),
}

// toolAction is the action a tool call was asked to perform, or "".
func toolAction(args any) string {
	if m, ok := args.(map[string]any); ok {
		action, _ := m["action"].(string)
		return action
	}
	return ""
}

// toolLabel is a friendly one-liner for a tool call: the tool's label plus the
// action it was asked to perform, when there is one. It is the English form;
// stepLabel words the same thing in a person's language.
func toolLabel(tool string, args any) string {
	return (voice{}).toolLabel(tool, toolAction(args))
}

// toolLabel is toolLabel in this voice's language. The action is the tool's own
// argument, so it stays as the tool spelled it.
func (v voice) toolLabel(tool, action string) string {
	label, ok := toolLabels[tool]
	if ok {
		label = msgcat.Translate(v.lang, label)
	} else {
		label = strings.ReplaceAll(tool, "_", " ")
		if label == "" {
			label = v.tr("Working")
		}
	}
	if action != "" {
		label += " (" + strings.ReplaceAll(action, "_", " ") + ")"
	}
	return truncate(label, 60)
}

// stepLabel is a card step's label in this voice's language.
func (v voice) stepLabel(step cardStep) string {
	if step.tool == "" {
		return step.label
	}
	return v.toolLabel(step.tool, step.action)
}

// --- per-person verbosity ---

func (st *UserState) verbosity() string {
	switch st.verb.Load() {
	case 1:
		return VerbosityQuiet
	case 2:
		return VerbosityVerbose
	}
	return VerbosityNormal
}

func (st *UserState) setVerbosity(v string) {
	switch normalizeVerbosity(v) {
	case VerbosityQuiet:
		st.verb.Store(1)
	case VerbosityVerbose:
		st.verb.Store(2)
	default:
		st.verb.Store(0)
	}
}

// postVerboseCall posts one silent line per LLM call for people who asked for
// verbose output.
func (s *BotService) postVerboseCall(pid int, c *cardLLM, payload map[string]any) {
	s.mu.RLock()
	var targets []*UserState
	for _, st := range s.users {
		if st.ActiveProjectID == pid && st.verbosity() == VerbosityVerbose && !st.Muted {
			targets = append(targets, st)
		}
	}
	s.mu.RUnlock()
	if len(targets) == 0 {
		return
	}

	mark := "🧠"
	if c.failed {
		mark = "⚠️"
	}
	logID := int(num(payload, "log_id"))
	safego.Go("bot-verbose-call", func() {
		for _, st := range targets {
			v := s.voice(st)
			line := fmt.Sprintf("%s #%d %s · %s · %s · %.1fs", mark, logID,
				strings.ToLower(str(payload, "stage")), truncate(c.model, 28), v.tr("%s in / %s out", fmtTokens(c.in), fmtTokens(c.out)), float64(c.durMs)/1000)
			if c.costKnown && !c.cacheHit {
				line += " · " + fmtCost(c.cost)
			}
			if names, ok := payload["tool_calls"].([]string); ok && len(names) > 0 {
				line += "\n" + truncate(strings.Join(names, ", "), 120)
			}
			kb, _ := buildInlineKeyboard([]string{v.tr("🔎 Details")}, []string{fmt.Sprintf("log:d:%d", logID)})
			if p, ok := s.provider(st.Provider); ok {
				_, _ = p.SendCard(st.ChatID, escapeHTML(line), kb, true)
				st.LiveCardStale.Store(true)
			}
		}
	})
}
