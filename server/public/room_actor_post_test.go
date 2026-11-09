package public

import (
	"testing"
	"time"
)

// A busy authoritative room must not silently drop lifecycle events: a lost @join
// strands a player outside the reducer's roster and a lost @leave leaves a ghost
// holding its seat, neither of which self-heals. post() blocks those on a full
// mailbox instead of dropping them, while high-frequency actions still drop so the
// read pump never wedges on a backed-up actor.
func TestRoomActorPostLifecycleBlocksInsteadOfDropping(t *testing.T) {
	a := &roomActor{key: "k", mailbox: make(chan actorMsg, 1), stopCh: make(chan struct{})}
	a.mailbox <- actorMsg{kind: amAction} // fill the single slot

	// An action on a full mailbox drops and returns at once.
	dropped := make(chan struct{})
	go func() { a.post(actorMsg{kind: amAction}); close(dropped) }()
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("post(amAction) on a full mailbox blocked; actions must drop, not block")
	}
	if len(a.mailbox) != 1 {
		t.Fatalf("a dropped action should leave the mailbox unchanged, got len %d", len(a.mailbox))
	}

	// A leave on a full mailbox blocks until a slot frees, then delivers.
	delivered := make(chan struct{})
	go func() { a.post(actorMsg{kind: amLeave, client: "c1"}); close(delivered) }()
	select {
	case <-delivered:
		t.Fatal("post(amLeave) returned while the mailbox was full; a lifecycle event must not drop")
	case <-time.After(50 * time.Millisecond):
		// still blocked, as required
	}
	<-a.mailbox // drain the filler, freeing one slot
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("post(amLeave) never delivered after a slot freed")
	}
	if got := <-a.mailbox; got.kind != amLeave || got.client != "c1" {
		t.Fatalf("expected the blocked amLeave to be delivered, got %+v", got)
	}
}

// A blocking lifecycle post must not hang forever when the actor is torn down:
// stopCh releases it so a disconnect during shutdown cannot wedge its goroutine.
func TestRoomActorPostUnblocksOnStop(t *testing.T) {
	a := &roomActor{key: "k", mailbox: make(chan actorMsg, 1), stopCh: make(chan struct{})}
	a.mailbox <- actorMsg{kind: amAction} // full

	done := make(chan struct{})
	go func() { a.post(actorMsg{kind: amJoin, client: "c2"}); close(done) }()
	select {
	case <-done:
		t.Fatal("post(amJoin) returned before a slot freed or the actor stopped")
	case <-time.After(50 * time.Millisecond):
	}
	close(a.stopCh) // tear down
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("post(amJoin) did not unblock when stopCh closed")
	}
}
