package engine

import (
	"log/slog"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/providers/llm"
)

// TestLiveReseedAppendsOverflowNote: the LIVE reseed reproduces the captured
// wake seed plus the deterministic overflow note. Setting a Reseed is what
// arms rollover and compaction for LIVE (both gate on Reseed != nil), so an
// overflowing live session reseeds instead of erroring at the ceiling.
func TestLiveReseedAppendsOverflowNote(t *testing.T) {
	wake := []llm.Message{
		{Role: llm.RoleUser, Content: "owner context"},
		{Role: llm.RoleUser, Content: "the task"},
	}
	out, err := liveReseed(wake)()
	if err != nil {
		t.Fatalf("liveReseed: %v", err)
	}
	if len(out) != len(wake)+1 {
		t.Fatalf("reseed should be seed+1 messages, got %d", len(out))
	}
	for i := range wake {
		if out[i].Content != wake[i].Content {
			t.Errorf("reseed message %d diverged from the captured seed", i)
		}
	}
	if out[len(out)-1].Content != liveOverflowNote {
		t.Errorf("reseed should end with the overflow note, got %q", out[len(out)-1].Content)
	}

	// Deterministic: a second reseed produces identical bytes.
	again, _ := liveReseed(wake)()
	for i := range out {
		if out[i].Content != again[i].Content {
			t.Fatalf("reseed is not deterministic at message %d", i)
		}
	}
}

// TestTrimChatThreadByteBudget: the carried thread is bounded by bytes as well
// as message count. Input is text-only (carryableThread output).
func TestTrimChatThreadByteBudget(t *testing.T) {
	// Under both caps: passthrough.
	small := []llm.Message{{Role: llm.RoleUser, Content: "hi"}}
	if got := trimChatThread(small); len(got) != 1 {
		t.Fatalf("small thread should pass through, got %d messages", len(got))
	}

	// A few huge messages blow the byte budget long before the 40-message cap.
	big := strings.Repeat("x", 20*1024)
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: big},
		{Role: llm.RoleAssistant, Content: big},
		{Role: llm.RoleUser, Content: big},
		{Role: llm.RoleAssistant, Content: "done"},
		{Role: llm.RoleUser, Content: "next"},
	}
	got := trimChatThread(msgs)
	if len(got) >= len(msgs) {
		t.Fatalf("byte budget did not evict anything (%d of %d kept)", len(got), len(msgs))
	}
	total := 0
	for _, m := range got {
		total += len(m.Content)
	}
	if total > maxChatThreadBytes {
		t.Errorf("kept thread is %d bytes, over the %d budget", total, maxChatThreadBytes)
	}
}

// carryableThread keeps only the conversational skeleton: the owner's messages
// and the model's prose. Tool results, tool calls, empty tool-call turns, and
// every engine-injected context block (owner, date, snapshots, overflow note)
// are dropped AT PERSIST TIME, so the carried bytes are identical wake over
// wake and the provider prefix cache never breaks mid-thread.
func TestCarryableThreadKeepsConversationOnly(t *testing.T) {
	wake := []llm.Message{
		ownerContextMessage("## Context\n- Owner: A\n"),
		{Role: llm.RoleUser, Content: snapshotFullSentinel + "\n<project_state>\nstuff\n</project_state>"},
		{Role: llm.RoleUser, Content: "[mode: owner-reported] the button is broken"},
		dateContextMessage(),
		{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{ID: "t1", Name: "manage_files", Arguments: `{"action":"get"}`}}},
		{Role: llm.RoleTool, Content: `{"success":true}`, ToolCallID: "t1"},
		{Role: llm.RoleAssistant, Content: "Fixing the handler now.", ToolCalls: []llm.ToolCall{{ID: "t2", Name: "manage_files", Arguments: `{"action":"patch"}`}}},
		{Role: llm.RoleTool, Content: `{"success":true}`, ToolCallID: "t2"},
		{Role: llm.RoleUser, Content: liveOverflowNote},
		{Role: llm.RoleAssistant, Content: "Done - the button works again."},
	}
	got := carryableThread(wake)
	want := []llm.Message{
		{Role: llm.RoleUser, Content: "[mode: owner-reported] the button is broken"},
		{Role: llm.RoleAssistant, Content: "Fixing the handler now."},
		{Role: llm.RoleAssistant, Content: "Done - the button works again."},
	}
	if len(got) != len(want) {
		t.Fatalf("carried %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Errorf("message %d = %s %q, want %s %q", i, got[i].Role, got[i].Content, want[i].Role, want[i].Content)
		}
		if len(got[i].ToolCalls) != 0 {
			t.Errorf("message %d still carries tool calls", i)
		}
	}

	// Idempotent: persisting a thread that already carried these messages must
	// not change a byte, or the next wake's cached prefix breaks for nothing.
	again := carryableThread(got)
	if len(again) != len(got) {
		t.Fatalf("carryableThread is not idempotent: %d -> %d messages", len(got), len(again))
	}
	for i := range got {
		if again[i].Content != got[i].Content {
			t.Errorf("idempotency broke at message %d", i)
		}
	}
}

// liveOutcomeWorker builds a minimal Worker backed by a real project DB and a
// bus subscriber that captures owner-facing chat messages.
func liveOutcomeWorker(t *testing.T) (*Worker, *[]string) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	bus := events.NewBus()
	var msgs []string
	bus.SubscribeAll(func(e events.Event) {
		if e.Type != events.EventEngineMessage {
			return
		}
		if c, ok := e.Payload["content"].(string); ok {
			msgs = append(msgs, c)
		}
	})
	w := &Worker{deps: &Deps{Bus: bus}, projectID: 1, pdb: pdb, logger: slog.Default()}
	return w, &msgs
}

// A live stop parks nothing - there is no pipeline to resume - so no
// phase_changed follows it. That makes stop_landed the ONLY signal the chat has
// to release its "Stopping…" hold on this path; without it the button sits
// disabled, which also suppresses Enter, leaving no way to send a message.
func TestPublishLiveOutcomeOwnerStopAnnouncesLanding(t *testing.T) {
	w, msgs := liveOutcomeWorker(t)
	var reasons []string
	w.deps.Bus.SubscribeAll(func(e events.Event) {
		if e.Type == events.EventEngineStopLanded {
			reason, _ := e.Payload["reason"].(string)
			reasons = append(reasons, reason)
		}
	})

	w.publishLiveOutcome(SessionResult{
		Ending:    EndPaused,
		EndDetail: endOwnerStop.detail,
		Paused:    true,
		ToolCalls: 2,
	}, nil)

	if len(reasons) != 1 || reasons[0] != "live" {
		t.Fatalf("stop_landed reasons = %v, want exactly one \"live\"", reasons)
	}
	if len(*msgs) != 1 {
		t.Fatalf("expected exactly 1 owner message on a stopped run, got %d: %v", len(*msgs), *msgs)
	}
}

// The announcement is specific to a stop: an ordinary pause parks the run and
// rides phase_changed, so announcing a landing there would release the hold for a
// stop that was never requested.
func TestPublishLiveOutcomeNonStopDoesNotAnnounceLanding(t *testing.T) {
	w, _ := liveOutcomeWorker(t)
	var landed int
	w.deps.Bus.SubscribeAll(func(e events.Event) {
		if e.Type == events.EventEngineStopLanded {
			landed++
		}
	})

	w.publishLiveOutcome(SessionResult{Ending: EndDone, ToolCalls: 1, Content: "All set."}, nil)

	if landed != 0 {
		t.Fatalf("stop_landed fired %d times on a clean run, want 0", landed)
	}
}

// A live run that exhausts a budget (the iteration cap) after doing real work
// must NOT go silent: the model never got a turn to send its closing summary, so
// the owner needs an explicit "applied it but ran out of steps" message.
// publishLiveOutcome must branch on exhausted-with-work; publishing nothing there
// presents to the owner as a stall after successful work.
func TestPublishLiveOutcomeExhaustedSendsMessage(t *testing.T) {
	w, msgs := liveOutcomeWorker(t)

	w.publishLiveOutcome(SessionResult{
		Ending:    EndExhausted,
		EndDetail: endIterationCap.detail,
		ToolCalls: 5,
		Content:   "Let me verify the page actually loads.", // trailing narration, not a summary
	}, nil)

	if len(*msgs) != 1 {
		t.Fatalf("expected exactly 1 owner message on a cut-short run, got %d: %v", len(*msgs), *msgs)
	}
}

// A give-up run must never present as a completed change. This exact shape - no
// closing content, tool calls landed - is what a clean finish also looks like, so
// the ending is the only thing separating "done" from "gave up": if a give-up
// ever collapses into EndDone, the owner is told the change is live for a session
// that produced nothing they asked for.
func TestPublishLiveOutcomeWedgeIsNotReportedAsDone(t *testing.T) {
	for _, e := range []ending{endWedged, endContinuationCap, endRepeatStall} {
		t.Run(e.detail, func(t *testing.T) {
			w, msgs := liveOutcomeWorker(t)

			w.publishLiveOutcome(SessionResult{
				Ending:    e.outcome,
				EndDetail: e.detail,
				ToolCalls: 3,
				Content:   "",
			}, nil)

			if len(*msgs) != 1 {
				t.Fatalf("expected exactly 1 owner message on a give-up run, got %d: %v", len(*msgs), *msgs)
			}
			// The failure this guards is a *false completion claim*, so assert on
			// meaning, not phrasing: whatever we say, it cannot claim success.
			for _, bad := range []string{"All done", "Done.", "Sorted", "nothing needed changing"} {
				if contains((*msgs)[0], bad) {
					t.Fatalf("give-up run reported as a completed change: %q", (*msgs)[0])
				}
			}
		})
	}
}

// Clean completion (EndDone) means the model already emitted its closing summary
// as assistant content during the loop, so publishLiveOutcome must add nothing -
// a second message would double up.
func TestPublishLiveOutcomeDoneStaysSilent(t *testing.T) {
	w, msgs := liveOutcomeWorker(t)

	w.publishLiveOutcome(SessionResult{
		Ending:    EndDone,
		EndDetail: endModelDone.detail,
		ToolCalls: 3,
		Content:   "Done. I centered the view on load.",
	}, nil)

	if len(*msgs) != 0 {
		t.Fatalf("clean completion must not publish an extra message, got %d: %v", len(*msgs), *msgs)
	}
}
