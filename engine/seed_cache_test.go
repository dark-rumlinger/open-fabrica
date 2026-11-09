package engine

import (
	"context"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
)

// A silent stop - OnTurnEnd returning ("", non-nil) - must end the session even
// though it injects nothing. Stop is honored independently of the `inject != ""`
// branch, or a silent-stop hook is ignored and the loop runs on.
func TestSessionOnTurnEndSilentStop(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "first")),
		enginetest.Calls(echoCall("c2", "second")), // would run if the stop were ignored
		enginetest.Calls(echoCall("c3", "third")),
	}}
	s := testSession(t, p)

	spec := buildSpecForTest()
	var consulted int
	spec.OnTurnEnd = func(iteration int, calls []llm.ToolCall, writeRejects int) (string, *ending) {
		consulted++
		return "", &endChecklistComplete // silent stop after the very first iteration
	}

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if consulted != 1 {
		t.Fatalf("OnTurnEnd should be consulted once before the silent stop, got %d (session did not stop)", consulted)
	}
	// OnTurnEnd's only caller stops when the checklist is complete, so a silent
	// stop is the model finishing on its own terms, not a give-up.
	if res.Ending != EndDone || res.EndDetail != endChecklistComplete.detail {
		t.Errorf("a silent stop must end the session as %s/%s, got %s/%s",
			EndDone, endChecklistComplete.detail, res.Ending, res.EndDetail)
	}
	if res.ToolCalls != 1 {
		t.Errorf("only the first iteration's tool call should run, got %d", res.ToolCalls)
	}
}

// The cached prompt prefix must stay byte-stable across calendar days: the
// volatile UTC date lives in its own trailing dateContextMessage, never in the
// owner-context block, and a carried live thread never persists the ephemeral
// blocks (carryableThread) so the next wake re-seeds exactly one fresh date.
func TestDateContextIsSeparateAndStrippable(t *testing.T) {
	dm := dateContextMessage()
	if !strings.HasPrefix(dm.Content, dateContextSentinel) {
		t.Fatalf("date message must carry the date sentinel, got %q", dm.Content)
	}
	if !strings.Contains(dm.Content, "Today (UTC)") {
		t.Errorf("date message should carry the UTC date line, got %q", dm.Content)
	}

	thread := []llm.Message{
		ownerContextMessage("## Context\n- Owner: A\n"),
		dateContextMessage(),
		{Role: llm.RoleAssistant, Content: "hi"},
		{Role: llm.RoleUser, Content: "real message"},
	}
	got := carryableThread(thread)
	if len(got) != 2 {
		t.Fatalf("expected the 2 non-ephemeral messages to survive, got %d", len(got))
	}
	for _, m := range got {
		if strings.HasPrefix(m.Content, ownerContextSentinel) || strings.HasPrefix(m.Content, dateContextSentinel) {
			t.Errorf("ephemeral context not stripped from carried thread: %q", m.Content)
		}
	}
}
