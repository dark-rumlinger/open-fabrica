package engine

import (
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// An owner message sent while a build runs is queued for the build session rather
// than the command channel (which drive does not read), and the build's turn hook
// hands it over at the next boundary.
func TestOwnerMessageDuringBuildReachesTheSession(t *testing.T) {
	w := &Worker{commands: make(chan Command, 1)}
	w.state = StateBuilding
	if err := w.Send(Command{Type: CommandChat, Payload: map[string]any{"message": "make the header blue"}}); err != nil {
		t.Fatal(err)
	}
	if len(w.commands) != 0 {
		t.Fatal("a mid-build chat must not wait in the command channel")
	}

	hook := w.withOwnerInbox(func(int, []llm.ToolCall, int) (string, *ending) { return "", nil })
	inject, stop := hook(0, nil, 0)
	if stop != nil || !strings.Contains(inject, ownerDuringBuildTag+" make the header blue") {
		t.Fatalf("the next turn must carry the owner's message, got %q (stop %v)", inject, stop)
	}
	if again, _ := hook(1, nil, 0); again != "" {
		t.Fatalf("a delivered message is not delivered twice, got %q", again)
	}
}

// A turn that ends the session leaves the message queued for the LIVE wake that
// follows the build, instead of injecting it where no further request would read it.
func TestOwnerMessageHeldWhenTheTurnStopsTheBuild(t *testing.T) {
	w := &Worker{}
	w.pushOwnerInbox("thanks!")
	hook := w.withOwnerInbox(func(int, []llm.ToolCall, int) (string, *ending) {
		return checklistCompleteStop, &endChecklistComplete
	})
	if inject, stop := hook(0, nil, 0); stop == nil || strings.Contains(inject, "thanks!") {
		t.Fatalf("a stopping turn must not consume the inbox, got %q", inject)
	}
	if msgs := w.takeOwnerInbox(); len(msgs) != 1 {
		t.Fatalf("the message must stay queued for the post-build wake, got %v", msgs)
	}
}
