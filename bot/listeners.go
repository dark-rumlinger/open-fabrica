package bot

import (
	"fmt"
	"strings"

	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/projectphase"
	"openfabrica/internal/safego"
)

// --- Event handlers (engine -> bot users) ---

func (s *BotService) onBrainMessage(e events.Event) {
	content, _ := e.Payload["content"].(string)
	if content == "" {
		return
	}
	// Don't relay intermediate build narration to external chat (tool_call lines).
	if tc, _ := e.Payload["tool_call"].(bool); tc {
		return
	}
	// The go-live message carries a hidden payload and phone-useless links; show
	// it as a purpose-built card instead.
	if info, ok := parseGoLive(content); ok {
		safego.Go("bot-golive", func() { s.announceGoLive(e.ProjectID, info) })
		return
	}
	// The expanded brief opens with a marker line instead of a heading, so each
	// person gets the heading in their own language.
	if brief, ok := strings.CutPrefix(content, engine.ExpandedBriefMarker); ok {
		body := mdToTelegramHTML(brief)
		if body == "" {
			return
		}
		safego.Go("bot-broadcast", func() {
			s.broadcastRender(e.ProjectID, func(v voice) string {
				return "<b>" + v.tr("Expanded brief") + "</b>\n\n" + body
			})
		})
		return
	}
	// LLM output is untrusted markup for Telegram's HTML parser: mdToTelegramHTML
	// escapes it before converting the few markdown forms the builder uses, so a
	// stray '<' or unbalanced tag can't make Telegram reject (and drop) the whole
	// message. Fire-and-forget so the engine's publishing goroutine isn't stalled
	// behind a synchronous HTTP send.
	safe := mdToTelegramHTML(content)
	if safe == "" {
		return
	}
	safego.Go("bot-broadcast", func() { s.broadcastToProjectUsers(e.ProjectID, safe) })
}

func (s *BotService) onQuestionAsked(e events.Event) {
	questionID := fmt.Sprintf("%v", e.Payload["id"])
	if questionID == "" || questionID == "<nil>" {
		return
	}
	safego.Go("bot-question-alert", func() { s.alertOthers(e.ProjectID, "question", "❓ <b>%s</b> has a question for you.") })

	// Snapshot the target users under the lock, then send outside it, detached
	// from the publishing goroutine. Holding s.mu across synchronous HTTP would
	// freeze the engine worker that published this event.
	s.mu.RLock()
	var targets []*UserState
	for _, state := range s.users {
		if state.ActiveProjectID == e.ProjectID {
			targets = append(targets, state)
		}
	}
	s.mu.RUnlock()
	if len(targets) == 0 {
		return
	}

	// The engine often asks a whole batch of questions at once (one event each).
	// Deliver them one at a time, surface the oldest pending question and skip if
	// the user is already answering one, so a free-text reply is never ambiguous
	// about which question it answers. The next question appears after each answer.
	safego.Go("bot-question", func() {
		for _, state := range targets {
			state.Mu.Lock()
			s.deliverNextIfIdle(state)
			state.Mu.Unlock()
		}
	})
}

func (s *BotService) onStageChange(e events.Event) {
	stage, _ := e.Payload["stage"].(string)
	if stage == "" {
		return
	}
	s.cardUpdate(e.ProjectID, func(st *cardState) { st.stage = stage })
	s.broadcastMilestone(e.ProjectID, func(v voice) string { return v.trh("Stage: %s", stage) })
}

// onPhaseChanged repaints the live card, which reads the phase when it renders,
// and announces the change.
func (s *BotService) onPhaseChanged(e events.Event) {
	phase, ok := e.Payload["phase"].(projectphase.Phase)
	if !ok || phase.Label == "" {
		return
	}
	s.cardUpdate(e.ProjectID, func(*cardState) {})
	s.broadcastMilestone(e.ProjectID, func(v voice) string { return v.trh("Now: %s", v.phaseLine(phase)) })
}

func (s *BotService) onBrainError(e events.Event) {
	errMsg, _ := e.Payload["error"].(string)
	if errMsg == "" {
		return
	}
	s.broadcastRender(e.ProjectID, func(v voice) string { return v.trh("Error: %s", errMsg) })
	safego.Go("bot-error-alert", func() { s.alertOthers(e.ProjectID, "error", "⚠️ <b>%s</b> ran into a problem.") })
}

func (s *BotService) onBrainStarted(e events.Event) {
	s.broadcastMilestone(e.ProjectID, func(v voice) string { return v.tr("Open Fabrica started.") })
}

func (s *BotService) onBrainStopped(e events.Event) {
	reason, _ := e.Payload["reason"].(string)
	s.cardUpdate(e.ProjectID, func(st *cardState) {
		st.stopped = true
		st.thinking = false
	})
	s.broadcastMilestone(e.ProjectID, func(v voice) string {
		msg := v.tr("Open Fabrica stopped.")
		if reason != "" {
			msg += " " + v.trh("Reason: %s", reason)
		}
		return msg
	})
}

func (s *BotService) onToolFailed(e events.Event) {
	tool, _ := e.Payload["tool"].(string)
	errMsg, _ := e.Payload["error"].(string)
	s.broadcastStatus(e.ProjectID, func(v voice) string { return v.trh("Tool failed: %s, %s", tool, errMsg) })
}

func (s *BotService) onWebhookReceived(e events.Event) {
	source, _ := e.Payload["source"].(string)
	s.broadcastStatus(e.ProjectID, func(v voice) string { return v.trh("Webhook received from: %s", source) })
}

func (s *BotService) onPaymentCompleted(e events.Event) {
	amount, _ := e.Payload["amount"].(string)
	s.broadcastStatus(e.ProjectID, func(v voice) string { return v.trh("Payment completed: %s", amount) })
}

func (s *BotService) onPaymentFailed(e events.Event) {
	reason, _ := e.Payload["reason"].(string)
	s.broadcastStatus(e.ProjectID, func(v voice) string { return v.trh("Payment failed: %s", reason) })
}

// notifyQueueSize bounds the notifications waiting to go out. A burst larger than
// this means Telegram is far behind; dropping the overflow beats growing without
// limit, and the live card still reflects the current state.
const notifyQueueSize = 512

// queued wraps an event handler so it runs on one background worker, in the order
// events arrived, instead of on the goroutine that published the event.
func (s *BotService) queued(fn func(events.Event)) func(events.Event) {
	s.notifyOnce.Do(func() {
		s.notifyQueue = make(chan func(), notifyQueueSize)
		safego.Go("bot-notify", func() {
			for task := range s.notifyQueue {
				safego.Run("bot-notify-task", task)
			}
		})
	})
	return func(e events.Event) {
		select {
		case s.notifyQueue <- func() { fn(e) }:
		default:
			s.logger.Warn("bot notification queue full; dropping an event", "type", e.Type)
		}
	}
}
