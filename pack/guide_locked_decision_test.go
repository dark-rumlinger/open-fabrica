package pack

import (
	"strings"
	"testing"
)

// A locked `## Decision:` block ships BUILD its unchosen branches and the
// plan-JSON selection matrix it can no longer act on (~2.6KB on a realtime
// build). RenderGuidesCoreLocked trims to the chosen branch while keeping the
// block's intro/trailer mechanics; anything unmatchable renders in full.
func TestRenderGuidesCoreLockedTrimsUnchosenBranches(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	var realtime []Pack
	for _, g := range Capabilities(ps) {
		if g.Slug == "realtime_signaling" {
			realtime = []Pack{g}
		}
	}
	if len(realtime) == 0 {
		t.Fatal("realtime_signaling guide not found")
	}

	full := RenderGuidesCore(realtime)
	locked := RenderGuidesCoreLocked(realtime, map[string]string{"realtime model": "tick"})

	if locked == full {
		t.Fatal("locking the decision changed nothing; the trim never ran")
	}
	if !strings.Contains(locked, `choice: "tick"`) {
		t.Error("the chosen tick branch was dropped")
	}
	// Fragments distinctive to the unchosen BULLETS (the trailer prose may
	// legitimately name other choices; only the branches themselves go).
	for _, gone := range []string{"auctions & bidding", "discrete messages ONLY"} {
		if strings.Contains(locked, gone) {
			t.Errorf("unchosen branch (fragment %q) still rendered", gone)
		}
	}
	if !strings.Contains(locked, "The other branches are omitted") {
		t.Error("trimmed block carries no omission note")
	}
	// The block's non-bullet mechanics are binding for BUILD and must survive.
	if !strings.Contains(locked, "run none of YOUR server-side code") {
		t.Error("the decision block's trailer mechanics were dropped with the branches")
	}
	// One vocabulary check outside the block: the rest of the core is untouched.
	if !strings.Contains(locked, "## System frames the server emits") {
		t.Error("content outside the decision block was disturbed")
	}

	// Determinism (the seed is cache-prefix material).
	if again := RenderGuidesCoreLocked(realtime, map[string]string{"realtime model": "tick"}); again != locked {
		t.Error("locked render is not deterministic")
	}
	// A nil map is byte-identical to the plain render (PLAN and the ratchets).
	if RenderGuidesCoreLocked(realtime, nil) != full {
		t.Error("nil locked map must render identically to RenderGuidesCore")
	}
	// An unmatchable choice falls back to the full block rather than guessing.
	odd := RenderGuidesCoreLocked(realtime, map[string]string{"realtime model": "no-such-model"})
	if !strings.Contains(odd, "auctions & bidding") {
		t.Error("a choice with no matching bullet must render the whole block")
	}
	// Every recordable choice carries its token, so a matchmaking lock trims too
	// rather than shipping all five branches to BUILD.
	mm := RenderGuidesCoreLocked(realtime, map[string]string{"realtime model": "matchmaking"})
	if strings.Contains(mm, "auctions & bidding") {
		t.Error("a matchmaking lock must trim the unchosen authoritative branch")
	}
	if !strings.Contains(mm, "manage_matchmaking") {
		t.Error("a matchmaking lock must keep its own bullet")
	}
}
