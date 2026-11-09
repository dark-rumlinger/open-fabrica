package engine

import (
	"strings"
	"testing"
)

// ownerDecision is a locked owner-scoped data-ownership decision (the shape that
// requires a login surface).
func ownerDecision() PlanDecision {
	return PlanDecision{Topic: "data ownership", Choice: "owner", Rationale: "each user sees only their own rows"}
}

// An owner-scoped decision with no login surface anywhere is a contradiction -
// the writes are dead on arrival and BUILD escapes by going public, discarding the
// ownership. It must be flagged.
func TestCoherence_OwnerScopeWithoutAuth_Flagged(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{ownerDecision()},
		Tasks: []PlanTaskSpec{
			{ID: "models", Title: "Models table + CRUD API", Paths: []string{"models"}},
			{ID: "page", Title: "My models page", Edits: []string{"/files/app.js"}},
		},
	}
	fb := planCoherenceFeedback(p, false)
	if fb == "" {
		t.Fatal("owner-scoped data with no auth surface must be flagged")
	}
	if !strings.Contains(fb, "login") || !strings.Contains(fb, "localStorage") {
		t.Errorf("feedback should name the login gap and the localStorage alternative: %q", fb)
	}
}

// An auth surface declared via a task's tools resolves the contradiction.
func TestCoherence_OwnerScopeWithAuthTool_Clean(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{ownerDecision()},
		Tasks: []PlanTaskSpec{
			{ID: "auth", Title: "User accounts", Tools: []string{"manage_auth"}},
			{ID: "models", Title: "Models table + CRUD API", Paths: []string{"models"}},
		},
	}
	if fb := ownerScopeWithoutAuthProblem(p); fb != "" {
		t.Errorf("an auth-surface task should clear the owner-scope problem, got %q", fb)
	}
}

// An auth surface declared only in prose (a login page task) also resolves it -
// planHasAuthSurface is generous so a coherent plan is never sent back.
func TestCoherence_OwnerScopeWithLoginPage_Clean(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{ownerDecision()},
		Tasks: []PlanTaskSpec{
			{ID: "login", Title: "Login page", Intent: "let users sign in and register an account"},
			{ID: "models", Title: "Models table", Paths: []string{"models"}},
		},
	}
	if fb := ownerScopeWithoutAuthProblem(p); fb != "" {
		t.Errorf("a login page should clear the owner-scope problem, got %q", fb)
	}
}

// `authored` (shared reads, owner-scoped writes) still needs an owner_column, and
// the runtime 401s an unauthenticated caller on any endpoint that sets one - so a
// collaborative table planned with no login surface is as dead on arrival as a
// private one, and must be flagged exactly like `owner`.
func TestCoherence_AuthoredScopeWithoutAuth_Flagged(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{{Topic: "data ownership", Choice: "authored"}},
		Tasks: []PlanTaskSpec{
			{ID: "messages", Title: "Channel messages table + CRUD API", Paths: []string{"messages"}},
		},
	}
	if fb := ownerScopeWithoutAuthProblem(p); fb == "" {
		t.Fatal("an authored-scoped table with no auth surface must be flagged - it 401s every write")
	}
}

// A public decision (no per-user scoping) never requires a login, so it is clean.
func TestCoherence_PublicOwnership_Clean(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{{Topic: "data ownership", Choice: "public"}},
		Tasks:     []PlanTaskSpec{{ID: "posts", Title: "Posts", Paths: []string{"posts"}}},
	}
	if fb := ownerScopeWithoutAuthProblem(p); fb != "" {
		t.Errorf("public ownership must be clean, got %q", fb)
	}
}

// No ownership decision at all -> nothing to check.
func TestCoherence_NoOwnershipDecision_Clean(t *testing.T) {
	p := &Plan{Tasks: []PlanTaskSpec{{ID: "page", Title: "Home"}}}
	if fb := ownerScopeWithoutAuthProblem(p); fb != "" {
		t.Errorf("a plan with no ownership decision must be clean, got %q", fb)
	}
}

// Many subsystem tasks editing one shared entry while owning no leaf of their
// own is the module-thrash shape (e.g. 6 tasks edit
// /files/builder.js, none own a leaf). It must be flagged.
func TestCoherence_SubsystemsWithoutLeaves_Flagged(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Spine", Paths: []string{"/", "/files/builder.js", "/files/builder.state.js"}},
			{ID: "catalogue", Title: "Catalogue", Edits: []string{"/files/builder.js"}},
			{ID: "place", Title: "Place tool", Edits: []string{"/files/builder.js"}},
			{ID: "erase", Title: "Erase tool", Edits: []string{"/files/builder.js"}},
			{ID: "slice", Title: "Slice", Edits: []string{"/files/builder.js"}},
		},
	}
	fb := planCoherenceFeedback(p, false)
	if fb == "" {
		t.Fatal("many subsystems funneled through one entry must be flagged")
	}
	if !strings.Contains(fb, "builder.js") || !strings.Contains(fb, "leaf") {
		t.Errorf("feedback should name the entry and the leaf remedy: %q", fb)
	}
}

// The CORRECT spine-plus-leaves layout - each subsystem owns its own leaf and only
// edits the entry - is clean.
func TestCoherence_SubsystemsWithLeaves_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Spine", Paths: []string{"/", "/files/builder.js", "/files/builder.state.js"}},
			{ID: "place", Title: "Place", Paths: []string{"/files/builder.place.js"}, Edits: []string{"/files/builder.js"}},
			{ID: "erase", Title: "Erase", Paths: []string{"/files/builder.erase.js"}, Edits: []string{"/files/builder.js"}},
			{ID: "slice", Title: "Slice", Paths: []string{"/files/builder.slice.js"}, Edits: []string{"/files/builder.js"}},
		},
	}
	if fb := subsystemsWithoutLeavesProblem(p); fb != "" {
		t.Errorf("per-subsystem leaves must be clean, got %q", fb)
	}
}

// The entry-exports anti-pattern: the spine owns the route AND the entry
// module yet declares the shared-state exports. Every leaf that edits the entry would
// import those symbols, closing a cycle - the plan-time catch for the save-gate's
// cyclic_import, which otherwise rejects the first leaf and forces a mid-build refactor.
func TestCoherence_EntryExports_Flagged(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js"},
				Exports: []string{"SCENE {scene,camera}", "GAME state store", "setMode(mode)"}},
			{ID: "procgen", Title: "Procgen", Paths: []string{"/files/game.procgen.js"}, Edits: []string{"/files/game.js"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	fb := planCoherenceFeedback(p, true)
	if !strings.Contains(fb, "unimported ROOT") || !strings.Contains(fb, "/files/game.state.js") {
		t.Fatalf("an entry task declaring exports must be flagged with the zero-import state-leaf fix, got: %q", fb)
	}
}

// One task owning BOTH the entry AND a zero-import store module, declaring the
// exports at the task level, is VALID: the exports home in the store (the un-edited
// owned module), which is the model's natural one-core-task plan shape. The entry
// is identified as the LEAF-EDITED module (game.js), never the trailing store
// (game.state.js) the task also owns.
func TestCoherence_EntryPlusStoreOneTask_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js", "/files/game.css", "/files/game.state.js"},
				Exports: []string{"renderView(name)", "setState(patch)", "getState()"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"}, Edits: []string{"/files/game.js"}},
			{ID: "hud", Title: "HUD", Paths: []string{"/files/game.hud.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	if fb := entryExportsProblem(p); fb != "" {
		t.Errorf("entry + store in one task must be clean (exports home in the store), got: %q", fb)
	}
	if home := taskExportHome(p, 0, "game.js"); home != "game.state.js" {
		t.Errorf("taskExportHome = %q, want game.state.js", home)
	}
}

// A store the leaves also EDIT is not a home: every owned client module being
// leaf-edited means the exports still have nowhere to live, so the flag fires.
func TestCoherence_EntryPlusEditedStore_Flagged(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js", "/files/game.state.js"},
				Exports: []string{"setState(patch)"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"},
				Edits: []string{"/files/game.js", "/files/game.state.js"}},
		},
	}
	if fb := entryExportsProblem(p); fb == "" {
		t.Fatal("exports with no un-edited home module must be flagged")
	}
}

// Clean: the shared state lives in a zero-import state-leaf task (game.state.js) and
// the entry declares no exports - the correct split shape. The state leaf owns no
// route, so it is not mistaken for the entry.
func TestCoherence_EntryWithStateLeaf_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app-spine", Title: "Entry", Paths: []string{"/", "/files/game.js"}},
			{ID: "state", Title: "State", Paths: []string{"/files/game.state.js"},
				Exports: []string{"SCENE {scene,camera}", "GAME state store"}},
			{ID: "flight", Title: "Flight", Paths: []string{"/files/game.flight.js"}, Edits: []string{"/files/game.js"}},
		},
	}
	if fb := entryExportsProblem(p); fb != "" {
		t.Errorf("exports on a zero-import state leaf (not the entry) must be clean, got: %q", fb)
	}
}

// The SAME module-thrash shape expressed over a page ROUTE - the shape a
// doctrine-following planner actually emits, since a front-end task lists the page
// route in `edits`, not an invented .js filename. This is a common plan
// shape: a spine owns /app and five subsystem tasks edit /app, none
// owning a leaf. A guard keyed only on .js entries would miss it and ship the
// monolith to BUILD, where it thrashes on the save-gates.
func TestCoherence_RouteFanoutWithoutLeaves_Flagged(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app", Title: "App shell", Paths: []string{"/app"}},
			{ID: "stream", Title: "Message stream", Edits: []string{"/app"}},
			{ID: "composer", Title: "Composer", Edits: []string{"/app"}},
			{ID: "threads", Title: "Threads", Edits: []string{"/app"}},
			{ID: "members", Title: "Channels & members", Edits: []string{"/app"}},
			{ID: "search", Title: "Search & profile", Edits: []string{"/app"}},
		},
	}
	fb := planCoherenceFeedback(p, false)
	if fb == "" {
		t.Fatal("five subsystems funneled through one page route must be flagged")
	}
	if !strings.Contains(fb, "/app") || !strings.Contains(fb, "leaf") {
		t.Errorf("feedback should name the route entry and the leaf remedy: %q", fb)
	}
}

// The corrected spine-plus-leaves layout for an app-like page - the spine owns the
// route AND the entry module AND the zero-import state leaf, and each subsystem owns
// its own leaf and only edits the entry - stays clean. This is exactly what the
// re-plan should produce, so the guard must NOT send it back again.
func TestCoherence_RouteWithLeaves_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "app", Title: "App shell", Paths: []string{"/app", "/files/app.js", "/files/app.state.js"}},
			{ID: "stream", Title: "Message stream", Paths: []string{"/files/app.stream.js"}, Edits: []string{"/files/app.js"}},
			{ID: "composer", Title: "Composer", Paths: []string{"/files/app.composer.js"}, Edits: []string{"/files/app.js"}},
			{ID: "threads", Title: "Threads", Paths: []string{"/files/app.threads.js"}, Edits: []string{"/files/app.js"}},
			{ID: "members", Title: "Members", Paths: []string{"/files/app.members.js"}, Edits: []string{"/files/app.js"}},
		},
	}
	if fb := subsystemsWithoutLeavesProblem(p); fb != "" {
		t.Errorf("per-subsystem leaves over a route must be clean, got %q", fb)
	}
}

// Two edit-only tasks on one entry is a legitimate small split (fits one save), so
// it stays under the threshold and is clean.
func TestCoherence_TwoEditsBelowThreshold_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Spine", Paths: []string{"/files/app.js"}},
			{ID: "a", Title: "A", Edits: []string{"/files/app.js"}},
			{ID: "b", Title: "B", Edits: []string{"/files/app.js"}},
		},
	}
	if fb := subsystemsWithoutLeavesProblem(p); fb != "" {
		t.Errorf("two edit tasks on one entry must be clean, got %q", fb)
	}
}

// Both problems in one plan surface together in a single corrective message.
func TestCoherence_BothProblems_Combined(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{ownerDecision()},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Spine", Paths: []string{"/files/app.js"}},
			{ID: "a", Title: "A", Edits: []string{"/files/app.js"}},
			{ID: "b", Title: "B", Edits: []string{"/files/app.js"}},
			{ID: "c", Title: "C", Edits: []string{"/files/app.js"}},
		},
	}
	fb := planCoherenceFeedback(p, false)
	if !strings.Contains(fb, "login") {
		t.Errorf("combined feedback should carry the owner-scope problem: %q", fb)
	}
	if !strings.Contains(fb, "leaf") {
		t.Errorf("combined feedback should carry the module-split problem: %q", fb)
	}
}

func TestCoherence_NilPlan_Clean(t *testing.T) {
	if fb := planCoherenceFeedback(nil, false); fb != "" {
		t.Errorf("nil plan must return no feedback, got %q", fb)
	}
}

// Update runs consult the live project: when it already has an auth surface, a
// re-plan that omits the auth task is NOT a contradiction and must pass - but
// the other checks stay on (a fresh subsystem funnel is flagged regardless).
func TestCoherence_ProjectHasAuth_SuppressesOnlyOwnerScope(t *testing.T) {
	ownerNoAuth := &Plan{
		Decisions: []PlanDecision{ownerDecision()},
		Tasks: []PlanTaskSpec{
			{ID: "models", Title: "Models table + CRUD API", Paths: []string{"models"}},
		},
	}
	if fb := planCoherenceFeedback(ownerNoAuth, true); fb != "" {
		t.Errorf("owner scope with live auth must be clean, got %q", fb)
	}

	funnel := &Plan{
		Decisions: []PlanDecision{ownerDecision()},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Spine", Paths: []string{"/files/app.js"}},
			{ID: "a", Title: "A", Edits: []string{"/files/app.js"}},
			{ID: "b", Title: "B", Edits: []string{"/files/app.js"}},
			{ID: "c", Title: "C", Edits: []string{"/files/app.js"}},
		},
	}
	fb := planCoherenceFeedback(funnel, true)
	if fb == "" {
		t.Fatal("a subsystem funnel must be flagged even when the project has auth")
	}
	if strings.Contains(fb, "login") {
		t.Errorf("the owner-scope problem must be suppressed when the project has auth: %q", fb)
	}
}

// An app-shaped decision with multiple screens still carved into their own
// routes is the SPA-duplicated-over-route-shells contradiction - flagged, and
// the feedback names the sprawling routes and the view-leaf fix.
func TestCoherence_AppShapeRouteSprawl_Flagged(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app", Rationale: "installable PWA"}},
		Tasks: []PlanTaskSpec{
			{ID: "today", Title: "Today screen", Paths: []string{"/", "/files/app.js", "/files/app.state.js"}},
			{ID: "form", Title: "Habit form screen", Paths: []string{"/habit/:slug", "/files/app.habit-form.js"}},
			{ID: "streaks", Title: "Streaks screen", Paths: []string{"/streaks/:slug", "/files/app.streaks.js"}},
			{ID: "settings", Title: "Settings screen", Paths: []string{"/settings", "/files/app.settings.js"}},
		},
	}
	fb := planCoherenceFeedback(p, false)
	if fb == "" {
		t.Fatal("app-shaped plan with extra screen routes must be flagged")
	}
	if !strings.Contains(fb, "/settings") || !strings.Contains(fb, "view leaf task") {
		t.Errorf("feedback should name the sprawling routes and the view-leaf fix: %q", fb)
	}
}

// The coherent app shape - one `/` page plus view leaf modules and the exempt
// standalone screens - is never sent back.
func TestCoherence_AppShapeSinglePage_Clean(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "App spine", Paths: []string{"/", "/files/app.js", "/files/app.state.js", "/files/app.css"}},
			{ID: "today", Title: "Today view", Paths: []string{"/files/app.today.js"}, Edits: []string{"/files/app.js"}},
			{ID: "settings", Title: "Settings view", Paths: []string{"/files/app.settings.js"}, Edits: []string{"/files/app.js"}},
			{ID: "login", Title: "Login page", Paths: []string{"/login"}},
		},
	}
	if fb := appShapeRouteSprawlProblem(p); fb != "" {
		t.Errorf("a single-page app shape must be clean, got %q", fb)
	}
}

// Conservative edges: a site-shaped (or undeclared) plan never fires, and ONE
// stray extra route on an app plan is a judgment call that is let through.
func TestCoherence_AppShapeEdges_Clean(t *testing.T) {
	site := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "site"}},
		Tasks: []PlanTaskSpec{
			{ID: "home", Title: "Home", Paths: []string{"/"}},
			{ID: "about", Title: "About", Paths: []string{"/about"}},
			{ID: "posts", Title: "Post detail", Paths: []string{"/posts/:slug"}},
		},
	}
	if fb := appShapeRouteSprawlProblem(site); fb != "" {
		t.Errorf("site-shaped plans never fire, got %q", fb)
	}
	undeclared := &Plan{Tasks: []PlanTaskSpec{
		{ID: "a", Title: "A", Paths: []string{"/a"}},
		{ID: "b", Title: "B", Paths: []string{"/b"}},
	}}
	if fb := appShapeRouteSprawlProblem(undeclared); fb != "" {
		t.Errorf("plans with no shape decision never fire, got %q", fb)
	}
	oneStray := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "App spine", Paths: []string{"/", "/files/app.js"}},
			{ID: "share", Title: "Public share view", Paths: []string{"/share/:slug"}},
		},
	}
	if fb := appShapeRouteSprawlProblem(oneStray); fb != "" {
		t.Errorf("one stray route is a judgment call and is let through, got %q", fb)
	}
}

// TestCoherence_AppShapeSprawl_FalseFires pins the two ways the sprawl check
// bounced a plan that is not sprawling.
func TestCoherence_AppShapeSprawl_FalseFires(t *testing.T) {
	// One route, listed by two tasks (a page and the task that edits it), is one
	// route. The threshold counts distinct routes, as the check's own "ONE stray
	// extra route is let through" rule requires.
	dupRoute := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "App spine", Paths: []string{"/", "/files/app.js"}},
			{ID: "about", Title: "About page", Paths: []string{"/about"}},
			{ID: "about-copy", Title: "About copy pass", Paths: []string{"/about"}},
		},
	}
	if fb := appShapeRouteSprawlProblem(dupRoute); fb != "" {
		t.Errorf("one route listed twice is not sprawl, got %q", fb)
	}

	// The standard legal pair is deliberately outside the app shell, like /404.
	legal := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "App spine", Paths: []string{"/", "/files/app.js"}},
			{ID: "privacy", Title: "Privacy", Paths: []string{"/privacy"}},
			{ID: "terms", Title: "Terms", Paths: []string{"/terms"}},
		},
	}
	if fb := appShapeRouteSprawlProblem(legal); fb != "" {
		t.Errorf("/privacy + /terms are exempt like /404, got %q", fb)
	}
}

// TestCoherence_AppShapeLock_SiteWithProseTail pins the substring match: the plan
// frame phrases this decision "site vs app", so a site choice that explains
// itself by naming the rejected option must not read as app-shaped.
func TestCoherence_AppShapeLock_SiteWithProseTail(t *testing.T) {
	site := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "site - routed pages, not a single-page app"}},
		Tasks: []PlanTaskSpec{
			{ID: "home", Title: "Home", Paths: []string{"/"}},
			{ID: "about", Title: "About", Paths: []string{"/about"}},
			{ID: "contact", Title: "Contact", Paths: []string{"/contact"}},
		},
	}
	if planLocksAppShape(site) {
		t.Error("a site choice that names 'app' in its rationale is still site-shaped")
	}
	if fb := appShapeRouteSprawlProblem(site); fb != "" {
		t.Errorf("route sprawl fired on a deliberately site-shaped plan, got %q", fb)
	}
}

// TestCoherence_AppShapeLock_StillLocksApp is the counterweight: the guard above
// must not turn a real app lock into a quiet no-op.
func TestCoherence_AppShapeLock_StillLocksApp(t *testing.T) {
	for _, choice := range []string{"app", "app-shaped", "app - one page at /, views switched client-side"} {
		p := &Plan{Decisions: []PlanDecision{{Topic: "project shape", Choice: choice}}}
		if !planLocksAppShape(p) {
			t.Errorf("choice %q should lock app shape", choice)
		}
	}
}

// realtimeTickDecision is a locked NON-authoritative realtime model - the shape that
// runs no server code, so a server reducer contradicts it.
func realtimeTickDecision() PlanDecision {
	return PlanDecision{Topic: "realtime model", Choice: "tick", Rationale: "continuous positions plus discrete chat on one endpoint"}
}

// realtimeModelContradictionProblem flags a non-authoritative realtime model
// (tick/relay) whose task relies on a server reducer, and leaves every other
// realtime shape clean.
func TestCoherence_RealtimeModel(t *testing.T) {
	tick := realtimeTickDecision()
	decision := func(choice string) *PlanDecision {
		return &PlanDecision{Topic: "realtime model", Choice: choice}
	}
	cases := []struct {
		name     string
		decision *PlanDecision // nil = no realtime decision
		task     PlanTaskSpec
		flagged  bool
	}{
		{"tick + reducer reliance", &tick, PlanTaskSpec{ID: "realtime", Title: "Tick endpoint",
			Intent: "the server reducer owns the player roster, assigns a colour on join, and expires idle players",
			Tools:  []string{"manage_tick"}}, true},
		{"tick + set_reducer", &tick, PlanTaskSpec{ID: "realtime",
			Intent: "set_reducer validates moves and expires idle players", Tools: []string{"manage_tick"}}, true},
		{"tick, reducer disclaimed", &tick, PlanTaskSpec{ID: "tick-endpoint", Title: "Tick websocket endpoint",
			Intent: "tick endpoint at tick_hz 30. NO server reducer - roster and presence come from net.onPeers and welcome/join/leave frames; name-tag colours assigned client-side on join",
			Tools:  []string{"manage_tick"}}, false},
		{"tick, no reducer prose", &tick, PlanTaskSpec{ID: "realtime", Title: "Non-authoritative tick endpoint",
			Intent: "clients net.send positions coalesced per tick; chat rides net.event; presence from net.onPeers",
			Tools:  []string{"manage_tick"}}, false},
		{"tick, without a reducer", &tick, PlanTaskSpec{ID: "realtime",
			Intent: "without a reducer; roster from welcome/join/leave frames"}, false},
		{"tick, reducer-less", &tick, PlanTaskSpec{ID: "realtime",
			Intent: "a reducer-less design, colours assigned client-side"}, false},
		{"tick, reducerless", &tick, PlanTaskSpec{ID: "realtime",
			Intent: "reducerless: nothing is server-authoritative"}, false},
		{"tick, drop the reducer", &tick, PlanTaskSpec{ID: "realtime",
			Intent: "drop the reducer and drive presence from net.onPeers"}, false},
		{"authoritative + reducer", decision("authoritative"), PlanTaskSpec{ID: "game",
			Intent: "set_reducer owns turns and scoring", Tools: []string{"manage_authoritative"}}, false},
		{"relay + authoritative tool", decision("relay"), PlanTaskSpec{ID: "chat",
			Intent: "server owns message ordering", Tools: []string{"manage_authoritative"}}, true},
		{"matchmaking + reducer", decision("matchmaking"), PlanTaskSpec{ID: "game",
			Intent: "the authoritative reducer settles the match", Tools: []string{"manage_authoritative"}}, false},
		{"no realtime decision", nil, PlanTaskSpec{ID: "x", Intent: "a reducer somewhere"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Plan{Tasks: []PlanTaskSpec{c.task, {ID: "other", Title: "Other", Paths: []string{"/files/other.js"}}}}
			if c.decision != nil {
				p.Decisions = []PlanDecision{*c.decision}
			}
			fb := realtimeModelContradictionProblem(p)
			switch {
			case c.flagged && fb == "":
				t.Fatal("want flagged, got clean")
			case c.flagged:
				if !strings.Contains(fb, planTaskSlug(c.task)) ||
					!strings.Contains(fb, "server-side reducer") || !strings.Contains(fb, "net.onPeers") {
					t.Errorf("flagged feedback should name the task and the fix: %q", fb)
				}
			case fb != "":
				t.Errorf("want clean, got %q", fb)
			}
		})
	}
}

// The case: a plan that spends a checklist item on an end-to-end
// verification sweep. It owns nothing and re-runs what the validators do for free.
func TestCoherence_PlatformOwnedVerificationTask_Flagged(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "home-page", Title: "Home page", Paths: []string{"/"}},
			{ID: "verification-pass", Title: "End-to-end verification",
				Intent: "Probe the running server: test_page on every route, test_endpoint on both APIs.",
				Deps:   []string{"home-page"}},
		},
	}
	fb := planCoherenceFeedback(p, true)
	if fb == "" {
		t.Fatal("a no-deliverable verification task must be flagged")
	}
	if !strings.Contains(fb, "verification-pass") {
		t.Errorf("feedback should name the offending task: %q", fb)
	}
}

// A task that owns real work is never flagged, whatever it is called: the check
// needs BOTH signals, and owning a path is proof it builds something.
func TestCoherence_PolishTaskThatOwnsWork_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "polish", Title: "Polish pass", Paths: []string{"/files/app.css"}},
			{ID: "e2e-page", Title: "End-to-end test dashboard", Edits: []string{"/files/app.js"}},
		},
	}
	if fb := planCoherenceFeedback(p, true); fb != "" {
		t.Errorf("tasks owning real work must not be flagged: %q", fb)
	}
}

// Plausible false-positive candidates: deliverable tasks that
// happen to declare no paths. Nothing in their titles reads as a process pass, so
// they must stay clean.
func TestCoherence_NoPathDeliverables_Clean(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "design-system", Title: "Global design system CSS"},
			{ID: "motion-module", Title: "Scroll reveal + page-transition module"},
			{ID: "seo-syndication", Title: "SEO meta, sitemap, robots"},
		},
	}
	if fb := planCoherenceFeedback(p, true); fb != "" {
		t.Errorf("path-less deliverables must not be flagged: %q", fb)
	}
}

// The Brew Board shape: a staff page behind a passcode, public tables, no login. The
// gate hides a page and protects nothing, so the plan is sent back; a login surface or
// a token-checking server function in front of the writes is coherent.
func TestPageOnlyGateProblem(t *testing.T) {
	gated := func() *Plan {
		return &Plan{Tasks: []PlanTaskSpec{
			{ID: "orders-table", Title: "Orders table", Intent: "orders table and a create_api over it", Paths: []string{"orders"}, Tools: []string{"manage_schema", "manage_api"}},
			{ID: "staff-page", Title: "Staff page", Intent: "baristas advance orders behind a staff passcode gate remembered on the device", Paths: []string{"/staff"}},
		}}
	}
	fb := planCoherenceFeedback(gated(), false)
	if !strings.Contains(fb, `"Staff page" gates a view behind a code`) || !strings.Contains(fb, "secret") {
		t.Fatalf("a page-only gate over public APIs must be sent back, got %q", fb)
	}

	withAuth := gated()
	withAuth.Tasks = append(withAuth.Tasks, PlanTaskSpec{Title: "Staff login", Intent: "staff log in", Tools: []string{"manage_auth"}})
	if fb := pageOnlyGateProblem(withAuth); fb != "" {
		t.Errorf("a login surface enforces the gate: %q", fb)
	}

	withFunction := gated()
	withFunction.Tasks = append(withFunction.Tasks, PlanTaskSpec{Title: "Advance endpoint", Intent: "a server function that verifies the staff token before it updates an order", Tools: []string{"manage_function"}})
	if fb := pageOnlyGateProblem(withFunction); fb != "" {
		t.Errorf("a token-checking server function enforces the gate: %q", fb)
	}

	if fb := planCoherenceFeedback(gated(), true); strings.Contains(fb, "gates a view") {
		t.Errorf("an update run on a project that already has auth is not flagged: %q", fb)
	}
}

// `authenticated` (anonymous submit, signed-in admin read) is a gated access model, so
// a plan that locks it must also plan the login the admin reads through.
func TestCoherence_AuthenticatedScopeWithoutAuth_Flagged(t *testing.T) {
	p := &Plan{
		Decisions: []PlanDecision{{Topic: "data ownership", Choice: "authenticated"}},
		Tasks: []PlanTaskSpec{
			{ID: "contact-table", Title: "Contact submissions table + API", Paths: []string{"submissions"}},
		},
	}
	if fb := ownerScopeWithoutAuthProblem(p); fb == "" {
		t.Fatal("an authenticated-model table with no auth surface must be flagged")
	}
}

// A task that loads a realtime wire tool needs the `realtime model` decision the plan
// prompt requires; without it nothing holds the build to the chosen model.
func TestCoherence_RealtimeToolWithoutDecision_Flagged(t *testing.T) {
	p := &Plan{Tasks: []PlanTaskSpec{{ID: "lobby", Title: "Lobby chat", Tools: []string{"manage_relay"}}}}
	if m := missingRealtimeDecisionProblem(p); m == "" {
		t.Fatal("a realtime tool with no realtime model decision must be flagged")
	}
	p.Decisions = []PlanDecision{{Topic: "realtime model", Choice: "relay"}}
	if m := missingRealtimeDecisionProblem(p); m != "" {
		t.Fatalf("a recorded decision satisfies the check, got %q", m)
	}
	plain := &Plan{Tasks: []PlanTaskSpec{{ID: "home", Title: "Home page", Tools: []string{"manage_schema"}}}}
	if m := missingRealtimeDecisionProblem(plain); m != "" {
		t.Fatalf("a plan with no realtime tool needs no decision, got %q", m)
	}
}
