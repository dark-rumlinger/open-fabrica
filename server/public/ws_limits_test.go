package public

import "testing"

func TestAcquireWSConnCapsPerVisitor(t *testing.T) {
	var releases []func()
	for i := 0; i < wsMaxConnsPerVisitor; i++ {
		rel, ok := acquireWSConn(900, "203.0.113.1")
		if !ok {
			t.Fatalf("connection %d refused below the cap", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := acquireWSConn(900, "203.0.113.1"); ok {
		t.Fatal("connection over the cap was admitted")
	}
	// Another visitor and another project have their own allowance.
	if rel, ok := acquireWSConn(900, "203.0.113.2"); !ok {
		t.Error("a different visitor was refused")
	} else {
		rel()
	}
	if rel, ok := acquireWSConn(901, "203.0.113.1"); !ok {
		t.Error("the same visitor on another project was refused")
	} else {
		rel()
	}
	// Releasing frees a slot, and a double release does not free two.
	releases[0]()
	releases[0]()
	if rel, ok := acquireWSConn(900, "203.0.113.1"); !ok {
		t.Fatal("slot not freed after release")
	} else {
		defer rel()
	}
	if _, ok := acquireWSConn(900, "203.0.113.1"); ok {
		t.Fatal("double release freed more than one slot")
	}
	for _, r := range releases[1:] {
		r()
	}
}

func TestWSReadLimit(t *testing.T) {
	if got := wsReadLimit(0); got != wsHardReadLimit {
		t.Errorf("unset max_msg_size: %d, want the hard ceiling %d", got, wsHardReadLimit)
	}
	if got := wsReadLimit(1000); got != 1000+16384 {
		t.Errorf("small cap: %d", got)
	}
	if got := wsReadLimit(1 << 30); got != wsHardReadLimit {
		t.Errorf("huge cap must clamp to %d, got %d", wsHardReadLimit, got)
	}
}
