package bot

import (
	"strconv"
	"strings"

	"openfabrica/db/models"
)

// handleCardAction runs a button on the live card. It returns the toast text.
func (s *BotService) handleCardAction(state *UserState, action string) string {
	v := s.voice(state)
	if state.ActiveProjectID == 0 || s.deps.Engine == nil {
		return v.tr("Pick a project first")
	}
	switch action {
	case "stop":
		if err := s.deps.Engine.RequestStop(state.ActiveProjectID); err != nil {
			return v.tr("Nothing is running")
		}
		return v.tr("Stopping at the next safe point")
	case "pause":
		if err := s.applyMode(state, models.ModePaused); err != nil {
			return v.tr("Couldn't pause")
		}
		return v.tr("Paused")
	case "resume":
		if err := s.applyMode(state, s.resumeMode(state.ActiveProjectID)); err != nil {
			return v.tr("Couldn't resume")
		}
		return v.tr("Resuming")
	}
	return ""
}

// handleLog routes log:<p|d|f>:<arg> callbacks.
func (s *BotService) handleLog(state *UserState, messageID int, rest string) string {
	if state.ActiveProjectID == 0 {
		v := s.voice(state)
		s.showScreen(state, messageID, s.mainMenuScreen(v))
		return v.tr("Pick a project first")
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ""
	}
	switch parts[0] {
	case "p":
		s.showScreen(state, messageID, s.logListScreen(state, int(n)))
	case "d":
		s.showScreen(state, messageID, s.logDetailScreen(state, n))
	case "f":
		return s.sendCallFile(state, n)
	}
	return ""
}

// alertsScreen lets a person choose how chatty the bot is.
func (s *BotService) alertsScreen(state *UserState) screen {
	v := s.voice(state)
	cur := state.verbosity()
	mark := func(level, label string) string {
		if cur == level {
			return "✓ " + label
		}
		return label
	}
	var now string
	switch cur {
	case VerbosityQuiet:
		now = v.tr("Quiet")
	case VerbosityVerbose:
		now = v.tr("Verbose")
	default:
		now = v.tr("Normal")
	}
	text := v.trh("🔔 <b>Alerts</b>\n\nHow much should I tell you while the builder works?\n\n"+
		"<b>Quiet</b>: replies, questions and errors only.\n"+
		"<b>Normal</b>: plus a live card that updates as the builder works.\n"+
		"<b>Verbose</b>: plus a line for every LLM call.\n\nNow: <b>%s</b>", now)
	return screen{
		text: text,
		labels: []string{
			mark(VerbosityQuiet, v.tr("🔕 Quiet")), mark(VerbosityNormal, v.tr("🔔 Normal")), mark(VerbosityVerbose, v.tr("📣 Verbose")),
			v.tr("◀ Back"), v.tr("☰ Menu"),
		},
		cbs: []string{"verb:quiet", "verb:normal", "verb:verbose", "nav:chat", "nav:menu"},
	}
}
