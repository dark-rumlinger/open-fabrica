package replayid

import "testing"

// A cookie only counts while its replay runs: an active token yields a per-visitor
// identity, and the same value is ignored once the replay ends or with an unknown
// token, so the cookie cannot be used to escape a limit.
func TestIdentity_OnlyWhileReplayActive(t *testing.T) {
	token, end := Begin()
	v := Value(token, "0.bob")
	if got, want := Identity(v), "replay:"+token+":0.bob"; got != want {
		t.Fatalf("Identity = %q, want %q", got, want)
	}
	if Identity(Value(token, "1.main")) == Identity(v) {
		t.Errorf("two visitors of one replay must not share an identity")
	}
	for _, bad := range []string{"", "nocolon", ":0.bob", token + ":", "deadbeef:0.bob"} {
		if got := Identity(bad); got != "" {
			t.Errorf("Identity(%q) = %q, want empty", bad, got)
		}
	}
	end()
	if got := Identity(v); got != "" {
		t.Errorf("an ended replay's cookie must not count, got %q", got)
	}
}
