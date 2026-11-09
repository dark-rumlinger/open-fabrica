package engine

// LIVE-mode behaviour: handleChatWake runs one session in response to an
// owner chat message on a deployed project; handleLiveResume folds an owner
// answer back into the live thread. There is no proactive periodic scan - the
// owner notices real issues faster than a speculative pass would.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"openfabrica/pack"
	"openfabrica/probe"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// handleChatWake runs one live session for an owner chat message. The
// in-memory chatThread carries the prior conversation across wakes so a short
// reply like "yes" still has its referent.
func (w *Worker) handleChatWake(ctx context.Context, userMessage string) {
	// Entering is a fresh intent to run: drop a stop left set by a prior session
	// that ended before the between-turns check could consume it (a stop landing
	// on the final no-tool turn), so it cannot end this run before it starts. A
	// stop clicked during THIS run lands after here and is still honoured. Mirrors
	// drive(); LIVE never passes through it.
	w.clearStopRequest()

	spec := w.liveWakeSpec(userMessage)

	sess := w.newStageSession(StageLive)
	res, err := w.runSpec(ctx, sess, spec)

	// Persist the post-run conversation for the next wake, reduced to its
	// conversational skeleton (carryableThread). Skip on error or pause - never
	// commit a half-finished thread.
	if err == nil && !res.Paused && len(res.Messages) > 0 {
		w.chatThread = trimChatThread(carryableThread(res.Messages))
	}

	w.publishLiveOutcome(res, err)
}

// liveOverflowNote is the deterministic marker a reseeded live segment carries
// so the model knows the prior attempt's tool context is gone.
const liveOverflowNote = "(the previous attempt overflowed the context; continue from project state, concisely)"

// liveReseed returns a Reseed over an already-captured wake seed: the original
// seed plus the overflow note. Pure over its captured input, which is what the
// rollover contract requires; setting it arms the rollover, so a long live
// session reseeds instead of erroring at the context ceiling. The mid-wake tool
// context is lost on a rollover, which an 18-iteration stage absorbs.
func liveReseed(wakeSeed []llm.Message) func() ([]llm.Message, error) {
	return func() ([]llm.Message, error) {
		out := make([]llm.Message, 0, len(wakeSeed)+1)
		out = append(out, wakeSeed...)
		out = append(out, llm.Message{Role: llm.RoleUser, Content: liveOverflowNote})
		return out, nil
	}
}

// liveWakeSpec assembles the session spec for one owner-chat wake.
func (w *Worker) liveWakeSpec(userMessage string) SessionSpec {
	spec := DefaultSpec(StageLive)
	spec.Guard = &RepeatGuard{}
	spec.System = w.liveSystemPrompt()
	// An empty PreloadTools would disable tools entirely (plan semantics); live
	// needs the find_tool surface, so preload the core editing set plus the
	// cross-artifact discovery surface (manage_codebase) live fixes reach for
	// first - keeps "where does X come from" off the list-then-read path.
	spec.PreloadTools = livePreloadTools()
	spec.OnModelDone = w.liveFlowCoverage(time.Now())

	framed := userMessage
	if len(w.chatThread) == 0 {
		framed = "[mode: owner-reported] " + userMessage
		// The first wake of a conversation learns what the last build left open; later
		// wakes already carry it in the thread.
		if w.pdb != nil {
			if known := knownIssuesBlock(w.pdb.Reader()); known != "" {
				framed = known + "\n" + framed
			}
		}
	}
	if note := notLiveNote(w.State()); note != "" {
		framed = note + "\n" + framed
	}

	spec.Seed = seed{
		owner:   w.ownerContext() + w.ownerMemoryBlock(),
		history: w.chatThread,
		task:    framed,
	}.messages()
	spec.Reseed = liveReseed(spec.Seed)
	return spec
}

// liveFlowCoverageNudge asks for a user flow covering a page the wake just added.
const liveFlowCoverageNudge = "You added %s in this change, but of_test.js has no flow that visits %s. Read the saved script (manage_files get of_test.js), then re-save it with manage_testing action=write_flows keeping every existing flow and adding one for the new page (visit it, assert its heading, follow its nav link). Then stop."

// liveFlowCoverage returns the OnModelDone hook for an owner-chat wake: when the model is
// about to stop after adding a page that no flow in of_test.js mentions, it is asked once
// for that flow. The live frame already says a new page gets its own flow, but a model
// finishing a small request skips it, and nothing else notices before the next build. A
// wake that adds no page, a project with user flows off, and a page an existing flow
// already visits all stay silent.
func (w *Worker) liveFlowCoverage(since time.Time) func() string {
	asked := false
	return func() string {
		if asked || w.pdb == nil || !w.userFlowsEnabled() {
			return ""
		}
		rows, err := w.pdb.Reader().Query(
			`SELECT path FROM of_files WHERE kind = 'page' AND is_deleted = 0 AND created_at >= ?`,
			since.UTC().Format("2006-01-02 15:04:05"))
		if err != nil {
			return ""
		}
		defer rows.Close()
		script := probe.LoadFlowSource(w.pdb.Reader())
		var missing []string
		for rows.Next() {
			var p string
			if rows.Scan(&p) != nil || p == "/404" || strings.Contains(p, ":") {
				continue
			}
			if !strings.Contains(script, p) {
				missing = append(missing, p)
			}
		}
		if rows.Err() != nil {
			return ""
		}
		if len(missing) == 0 {
			return ""
		}
		asked = true
		names := strings.Join(missing, ", ")
		w.logger.Info("live: new page has no user flow, asking for one", "pages", names)
		pronoun := "it"
		if len(missing) > 1 {
			pronoun = "them"
		}
		return fmt.Sprintf(liveFlowCoverageNudge, names, pronoun)
	}
}

// livePreloadTools is the LIVE-stage preload set: the build core and data backbone plus
// the live-only discovery tools. The one source for every LIVE-prompt session (owner chat
// and scheduled tasks) AND for liveSystemPrompt, whose routing prose and deferred index
// are computed from it: a session preloading less than its prompt advertises is told a
// tool is loaded that is not.
func livePreloadTools() []string {
	return append(buildPreloadTools(Task{}, true), tools.LiveExtraToolNames...)
}

// liveSystemPrompt renders the LIVE system prompt: the always-on platform layer
// (mental model + laws + guide index) plus the live tool catalogue.
func (w *Worker) liveSystemPrompt() string {
	// Guarded like buildSystemPrompt so a registry-only Worker (tests) can
	// render; a nil DB yields zero caps, listing only ungated tools.
	var caps llm.Capabilities
	if w.deps != nil && w.deps.DB != nil {
		var err error
		caps, err = llm.LoadCapabilities(w.deps.DB.DB)
		if err != nil && w.logger != nil {
			w.logger.Warn("load capabilities failed", "error", err)
		}
	}
	sel := pack.Select(w.effectivePacks(), StageLive)
	// The preloaded set's routing prose. A preloaded tool reaches the model as a wire
	// schema whose description is its 130-char one-liner, and the only other path that
	// emits WhenToUse is the find_tool handler - which the model is told not to use on
	// these. The set is fixed, so this rides the cached prefix.
	extra := ""
	preloaded := livePreloadTools()
	if prose := w.deps.ToolRegistry.RoutingProse(preloaded, caps, StageLive); prose != "" {
		extra = "## Your always-loaded tools\n\n" + prose
	}
	return pack.Assemble(pack.AssembleInput{
		Stage:     StageLive,
		Wave:      pack.Wave,
		Selection: sel,
		// DeferredIndex, not Index: a preloaded tool already arrives with its full wire
		// schema AND its routing prose above, so listing it a third time in the catalogue
		// bills the same names and verbs again on every LIVE turn.
		ToolIndex:    w.deps.ToolRegistry.DeferredIndex(preloaded, caps, StageLive),
		Extra:        extra,
		HiddenGuides: capabilityDeadGuides(w.effectivePacks(), w.deps.ToolRegistry, caps),
	}).System
}

// publishLiveOutcome turns a finished live run into owner-facing messages.
func (w *Worker) publishLiveOutcome(res SessionResult, err error) {
	switch {
	case err == nil && res.EndDetail == endOwnerStop.detail:
		// An owner stop in LIVE parks nothing: there is no pipeline to resume,
		// so the worker stays live and ready for the next message. Checked ahead
		// of the pause branch, which this one shares an Ending with.
		w.clearStopRequest()
		w.publishMessage(pickPhrase(
			"Stopped there. Tell me what you'd like instead.",
			"Alright, I've stopped. What would you like me to do?",
		))
		// The only landing signal this path has: no run is parked, so no
		// phase_changed follows to release the chat's "Stopping…" hold.
		w.publishStopLanded("live")
	case err == nil && res.Paused:
		w.publishMessage(pickPhrase(
			"Just need your answer before I carry on.",
			"I'll continue as soon as you let me know.",
		))
		w.stampLiveQuestions()
		w.pauseAwaitingAnswers()
	case err != nil:
		w.logger.Error("live run failed", "error", err)
		w.publishMessage(pickPhrase(
			"Hit a snag handling that. Mind trying again or rephrasing?",
			"That didn't go through. Try again or rephrase?",
		))
	case res.Ending == EndFailed:
		// The loop could not make progress: the model stopped producing usable
		// turns, or stalled repeating itself. Some work may have landed, but it
		// never confirmed the change, so this must never fall through to a
		// completion message. Checked before the Content cases because a give-up is
		// worth saying even when a fragment was streamed.
		w.publishMessage(pickPhrase(
			"I got stuck partway through that one and stopped rather than guess. Tell me to try again, or rephrase what you're after and I'll have another go.",
			"That one tied me in knots and I stopped before making a mess. Have another go at describing it and I'll pick it back up.",
		))
	case res.Ending == EndExhausted && res.ToolCalls > 0:
		// A budget ran out mid-fix: work happened, but the model never got a turn
		// to send its closing summary. Don't go silent - tell the owner the change
		// landed but the run was cut short.
		w.publishMessage(pickPhrase(
			"I've applied the change, but ran out of steps before I could wrap up. Tell me to continue if anything still looks off.",
			"That's updated, though I hit my step limit before finishing the check. Let me know if it's not quite right and I'll carry on.",
		))
	case res.Content == "" && res.ToolCalls > 0:
		w.publishMessage(pickPhrase(
			"All done, the change is live.",
			"Done. That's updated for you.",
			"Sorted, the change has been applied.",
		))
	case res.Content == "" && res.ToolCalls == 0:
		w.publishMessage(pickPhrase(
			"Took a look, nothing needed changing here.",
			"Checked this over, it already looks right.",
			"Had a look, no change needed.",
		))
	}
}

// handleLiveResume re-enters the live agent after the owner answered a
// question it asked.
func (w *Worker) handleLiveResume(ctx context.Context) {
	answer := w.takeWakeAnswer()
	if answer == "" {
		answer = "(the owner answered)"
	}
	w.handleChatWake(ctx, "The owner answered your question: "+answer)
}

// carryableThread reduces a finished wake's transcript to its conversational
// skeleton - the only part worth carrying into the next wake's prompt:
//
//   - RoleTool results and assistant tool CALLS are dropped: they are stale by
//     the next wake (project_state re-seeds fresh state), and carrying them made
//     the thread mostly tool bulk that rode - and re-billed - on every wake.
//   - Engine-injected context is dropped: the owner block and date are re-seeded
//     fresh each wake, project_state snapshots go stale, and the overflow note is
//     session mechanics, not conversation.
//
// Stripping at PERSIST time (not seed time) keeps the carried bytes identical
// from wake to wake, so the provider prefix cache serves the whole thread
// instead of breaking at a message removed or rewritten mid-thread. What
// survives is the owner's messages and the model's own prose - the referent a
// short reply like "yes" needs.
func carryableThread(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == llm.RoleTool:
			continue
		case m.Role == llm.RoleAssistant:
			if strings.TrimSpace(m.Content) == "" {
				continue // a pure tool-call turn carries no prose
			}
			out = append(out, llm.Message{Role: llm.RoleAssistant, Content: m.Content})
		case m.Role == llm.RoleUser && isEngineContextMessage(m.Content):
			continue
		default:
			out = append(out, m)
		}
	}
	return out
}

// isEngineContextMessage reports whether a user-role message is engine-injected
// context rather than conversation: the owner/date blocks, a project_state
// snapshot or delta, or the rollover overflow note.
func isEngineContextMessage(content string) bool {
	return strings.HasPrefix(content, ownerContextSentinel) ||
		strings.HasPrefix(content, dateContextSentinel) ||
		strings.HasPrefix(content, snapshotBaselineSentinel) ||
		strings.HasPrefix(content, snapshotDeltaSentinel) ||
		strings.HasPrefix(content, snapshotFullSentinel) ||
		content == liveOverflowNote
}

// maxChatThreadBytes bounds the carried live thread by content size, not just
// message count. The thread is text-only (carryableThread), so this cap is a
// backstop for very long conversations rather than the working limit it was
// when tool traffic rode along.
const maxChatThreadBytes = 60 * 1024

// trimChatThread caps the carried live thread by message count and by bytes.
// Input is carryableThread output (text-only user/assistant messages), so a cut
// can never orphan a tool result. A cut breaks the next wake's cached prefix
// once, which is why the caps are generous: text-only threads rarely hit them.
func trimChatThread(msgs []llm.Message) []llm.Message {
	const maxKeep = 40
	start := 0
	if len(msgs) > maxKeep {
		start = len(msgs) - maxKeep
	}
	total := 0
	for i := len(msgs) - 1; i >= start; i-- {
		total += len(msgs[i].Content)
		if total > maxChatThreadBytes {
			start = i + 1
			break
		}
	}
	if start == 0 {
		return msgs
	}
	out := make([]llm.Message, len(msgs)-start)
	copy(out, msgs[start:])
	return out
}

// notLiveNote tells a wake on a project that is not live which state it is in: the
// owner chats with the same agent whether or not the build is running, and the LIVE
// frame cannot assume a deployed project. Empty for a live project.
func notLiveNote(st State) string {
	switch st {
	case StatePaused:
		return "(Project status: the build is paused. Answer, or make a requested change; the build picks up the current state when it resumes.)"
	case StateIdle:
		return "(Project status: no build has run yet. Answer questions; a request to build the project starts from the brief, not from chat edits.)"
	case StateError:
		return "(Project status: the last build stopped on an error. Answer, or fix what the owner asks; they can restart the build afterwards.)"
	}
	return ""
}
