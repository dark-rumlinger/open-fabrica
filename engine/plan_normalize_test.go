package engine

import (
	"strings"
	"testing"
)

// The common slip: a spine owns the route AND the entry module and declares the
// shared-state exports, with no store module to home them. normalizeEntryExports
// must add the conventional store module to that task's paths, and the coherence
// guard must then be silent - no LLM re-plan turn spent.
func TestNormalizeEntryExports_AddsStoreModule(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js"},
				Exports: []string{"SCENE {scene,camera}", "GAME state store", "setMode(mode)"}},
			{ID: "procgen", Title: "Procgen", Paths: []string{"/files/game.procgen.js"}, Edits: []string{"/files/game.js"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	// Guard fires before normalization.
	if entryExportsProblem(p) == "" {
		t.Fatal("precondition: guard should flag the un-homed entry exports")
	}

	fixes := normalizeEntryExports(p)
	if len(fixes) != 1 {
		t.Fatalf("expected 1 fix applied, got %d: %v", len(fixes), fixes)
	}
	if !strings.Contains(fixes[0], "/files/game.state.js") {
		t.Errorf("fix line should name the added store module: %q", fixes[0])
	}
	// The store module is now owned by the spine.
	if got := taskExportHome(p, 0, "game.js"); got != "game.state.js" {
		t.Errorf("after normalize, taskExportHome = %q, want game.state.js", got)
	}
	// And the guard is silent - no re-plan turn needed.
	if fb := entryExportsProblem(p); fb != "" {
		t.Errorf("guard must be silent after normalization, got: %q", fb)
	}
}

// Idempotent and non-firing on an already-valid plan (spine owns entry + store).
func TestNormalizeEntryExports_NoopWhenStorePresent(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js", "/files/game.state.js"},
				Exports: []string{"renderView(name)", "getState()"}},
			{ID: "hud", Title: "HUD", Paths: []string{"/files/game.hud.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Fatalf("valid plan must not be touched, got fixes: %v", fixes)
	}
	// Second pass is still a no-op.
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Fatalf("normalization must be idempotent, got fixes: %v", fixes)
	}
}

// Ambiguity guard: when the derived store path is already claimed by another task
// (owned or edited), the engine must NOT auto-add it - it leaves the plan for the
// LLM repair, and the coherence guard still fires.
func TestNormalizeEntryExports_SkipsWhenStorePathClaimed(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js"},
				Exports: []string{"getState()"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"}, Edits: []string{"/files/game.js"}},
			// Another task already owns the conventional store path: adding it to the
			// spine would collide, so normalization must back off.
			{ID: "state", Title: "State", Paths: []string{"/files/game.state.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Fatalf("must not auto-add a claimed store path, got fixes: %v", fixes)
	}
	if entryExportsProblem(p) == "" {
		t.Error("guard should still fire so the LLM repair resolves the ambiguity")
	}
}

// A leaf/data task that declares exports but owns no page route is not a spine and
// must be left alone (the store-in-its-own-task shape).
func TestNormalizeEntryExports_IgnoresNonSpine(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Entry", Paths: []string{"/", "/files/game.js"}},
			{ID: "store", Title: "Store", Paths: []string{"/files/game.state.js"},
				Exports: []string{"getState()"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Fatalf("a non-spine export task must be left alone, got fixes: %v", fixes)
	}
}

// The spine owns the route and the STORE (`board.state.js`), and the leaves
// edit that store to add their exports. The store is not the entry: normalization must
// not derive a second store `board.state.state.js` from it, and the guard stays quiet.
func TestNormalizeEntryExports_StoreEditedByLeavesIsNotTheEntry(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "board-page", Title: "Board", Paths: []string{"/", "/files/board.state.js"},
				Exports: []string{"subscribe(fn)", "getOrders()", "applyEvent(e)"}},
			{ID: "board-render", Title: "Render", Paths: []string{"/files/board.render.js"}, Edits: []string{"/files/board.state.js"}},
			{ID: "board-pulse", Title: "Pulse", Paths: []string{"/files/board.pulse.js"}, Edits: []string{"/files/board.state.js"}},
		},
	}
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Fatalf("a store edited by leaves must not be taken for the entry, got fixes: %v", fixes)
	}
	for _, pth := range p.Tasks[0].Paths {
		if strings.Contains(pth, ".state.state") {
			t.Fatalf("derived a doubled store path: %v", p.Tasks[0].Paths)
		}
	}
	if fb := entryExportsProblem(p); fb != "" {
		t.Errorf("guard must be silent: the exports home in the store, got %q", fb)
	}
	if got := stateLeafPath("/files/board.state.js"); got != "/files/board.state.js" {
		t.Errorf("stateLeafPath must be idempotent on a store path, got %q", got)
	}
}

// Leaves wire in through deps and imports, so no task EDITS the entry. The
// spine still owns the entry and declares exports; it must be recognised as the entry
// all the same - normalized to a store when that path is free.
func TestNormalizeEntryExports_DepsWiredEntryGetsStore(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Spine", Paths: []string{"/", "/files/badsanta.js", "/files/badsanta.css"},
				Exports: []string{"mountRegion(name) -> element"}},
			{ID: "thread", Title: "Thread", Paths: []string{"/files/badsanta.thread.js"}, Deps: []string{"app-spine"}},
		},
	}
	if entryExportsProblem(p) == "" {
		t.Fatal("precondition: a deps-wired entry with exports must trip the guard")
	}
	fixes := normalizeEntryExports(p)
	if len(fixes) != 1 || !strings.Contains(fixes[0], "/files/badsanta.state.js") {
		t.Fatalf("expected the store module added, got %v", fixes)
	}
	if fb := entryExportsProblem(p); fb != "" {
		t.Errorf("guard must be silent after normalization, got %q", fb)
	}
}

// When a deps-wired spine's derived store is already another task's, the engine does
// not guess: the guard fires so the re-plan moves the exports. Passing silently would
// let the checklist tell every leaf to import from the entry.
func TestEntryExportsProblem_DepsWiredStoreClaimed(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "state-store", Title: "Store", Paths: []string{"/files/badsanta.state.js"}, Exports: []string{"subscribe(fn)"}},
			{ID: "app-spine", Title: "Spine", Paths: []string{"/", "/files/badsanta.js"},
				Exports: []string{"mountRegion(name) -> element", "store"}, Deps: []string{"state-store"}},
			{ID: "thread", Title: "Thread", Paths: []string{"/files/badsanta.thread.js"}, Deps: []string{"app-spine"}},
		},
	}
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Fatalf("a claimed store path must not be auto-added, got %v", fixes)
	}
	if entryExportsProblem(p) == "" {
		t.Error("guard must fire for exports on a deps-wired entry")
	}
}

// A lone single-module page (no split) may declare exports: nothing imports them, so
// neither the guard nor the normalizer touches it.
func TestEntryExportsProblem_UnsplitPageLeftAlone(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "page", Title: "Page", Paths: []string{"/", "/files/app.js"}, Exports: []string{"render()"}},
			{ID: "data", Title: "Data", Paths: []string{"orders"}},
		},
	}
	if fb := entryExportsProblem(p); fb != "" {
		t.Errorf("an unsplit page must not be flagged, got %q", fb)
	}
	if fixes := normalizeEntryExports(p); len(fixes) != 0 {
		t.Errorf("an unsplit page must not be normalized, got %v", fixes)
	}
}
