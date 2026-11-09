package replayoutbox

import "testing"

func TestCaptureOnlyWhileAReplayRuns(t *testing.T) {
	if Capture(7, Message{Kind: "email", To: "a@b.c"}) {
		t.Fatal("nothing is captured outside a replay")
	}
	end := Begin(7)
	inner := Begin(7)
	if !Capture(7, Message{Kind: "email", To: "a@b.c", Subject: "Hi"}) || !Active(7) {
		t.Fatal("a running replay captures")
	}
	if Capture(8, Message{Kind: "push"}) {
		t.Fatal("another project's messages are not captured")
	}
	inner()
	inner() // idempotent
	if got := Messages(7); len(got) != 1 || got[0].Subject != "Hi" || got[0].At.IsZero() {
		t.Fatalf("messages = %+v", got)
	}
	end()
	if Active(7) || Messages(7) != nil {
		t.Fatal("the outbox is dropped when the last replay ends")
	}
}
