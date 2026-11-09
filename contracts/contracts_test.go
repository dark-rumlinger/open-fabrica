package contracts

import "testing"

// wantIDs is the set of contracts the platform ships - the single home is
// AllIDs (contracts.go); a deleted/renamed file is caught because its constant
// no longer resolves.
var wantIDs = AllIDs

func TestLoadAllContracts(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m) != len(wantIDs) {
		t.Fatalf("loaded %d contracts, want %d", len(m), len(wantIDs))
	}
	for _, id := range wantIDs {
		c, ok := m[id]
		if !ok {
			t.Errorf("missing contract %q", id)
			continue
		}
		if c.Title == "" {
			t.Errorf("contract %q has no title", id)
		}
		if Render(id) == "" {
			t.Errorf("contract %q renders empty", id)
		}
	}
}

func TestRenderDeterministic(t *testing.T) {
	for _, id := range wantIDs {
		if a, b := Render(id), Render(id); a != b {
			t.Errorf("Render(%q) not deterministic", id)
		}
	}
	// List order is stable (sorted by ID).
	l1, _ := List()
	l2, _ := List()
	for i := range l1 {
		if l1[i].ID != l2[i].ID {
			t.Fatalf("List order not deterministic at %d: %q vs %q", i, l1[i].ID, l2[i].ID)
		}
	}
}

func TestRenderUnknownIsEmpty(t *testing.T) {
	if got := Render("does-not-exist"); got != "" {
		t.Errorf("Render(unknown) = %q, want empty", got)
	}
}

// TestMustRenderPanicsOnUnknown: a code-driven reference to a renamed/mistyped
// contract must fail loudly, not inline nothing.
func TestMustRenderPanicsOnUnknown(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustRender(unknown) did not panic")
		}
	}()
	_ = MustRender("does-not-exist")
}

func TestMatchesProbes(t *testing.T) {
	m, _ := Load()
	c := m["auth-window-api"]
	if !c.Matches("the storage key is exactly auth_token here") {
		t.Errorf("expected match when probe token present")
	}
	if c.Matches("no token key mentioned") {
		t.Errorf("expected no match when probe token absent")
	}
	// A contract with no probes never claims a match.
	if m["page-layout"].Matches("anything at all") {
		t.Errorf("contract with no probes should never match")
	}
}
