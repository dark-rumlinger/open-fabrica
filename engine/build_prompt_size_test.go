package engine

import (
	"encoding/json"
	"testing"

	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// buildFloorBytes measures the BUILD static floor for one plan: the bytes re-sent on every
// iteration before a single tool result lands, including the registry-derived blocks and
// tool schemas that the pack frame-size budgets hold out.
func buildFloorBytes(t *testing.T, plan *Plan) (sys string, schemas []byte, defs []llm.ToolDef) {
	t.Helper()
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	sys = w.buildSystemPrompt(plan)
	defs = w.deps.ToolRegistry.LookupFull(buildAllTools(plan), llm.Capabilities{}, StageBuild)
	defs = append(defs, tools.FindToolDef())
	var err error
	schemas, err = json.Marshal(defs)
	if err != nil {
		t.Fatalf("marshal tool schemas: %v", err)
	}
	return sys, schemas, defs
}

// pagesOnlyPlan owns page routes and the shared layout, no table or CRUD resource, so the
// data-backbone tier stays deferred. tablePlan adds one plain table task, which pulls it in.
var (
	pagesOnlyPlan = &Plan{Goal: "a content site", Tasks: []PlanTaskSpec{
		{ID: "shell", Title: "Shared layout", Intent: "header, nav, footer", Paths: []string{"default"}},
		{ID: "home", Title: "Home page", Intent: "landing page with a hero", Paths: []string{"/"}},
	}}
	tablePlan = &Plan{Goal: "a link tracker", Tasks: []PlanTaskSpec{
		{ID: "links", Title: "Links table", Intent: "store saved links", Paths: []string{"links"}},
		{ID: "home", Title: "Home page", Intent: "list the links", Paths: []string{"/"}},
	}}
	// loadedPlan is the shape a real multi-page brief produces: the backbone tier plus
	// the handful of capability tools a site with auth, an admin area, email and syndication
	// pulls in. Sized like a realistic large plan (~17 tasks, ~13 resolved tools). The two plans
	// above are minimal by design and so miss the floor that ships.
	loadedPlan = &Plan{Goal: "a studio site with an admin area", Tasks: []PlanTaskSpec{
		{ID: "projects", Title: "Projects table + read API", Intent: "store portfolio projects", Paths: []string{"projects"}},
		{ID: "submissions", Title: "Contact submissions table + POST API", Intent: "store contact messages", Paths: []string{"submissions"}},
		{ID: "shell", Title: "Shared layout", Intent: "header, nav, footer", Paths: []string{"default"}},
		{ID: "home", Title: "Home page", Intent: "landing page with a hero", Paths: []string{"/"}},
		{ID: "work", Title: "Work page", Intent: "filterable project list", Paths: []string{"/work"}},
		{ID: "contact", Title: "Contact page", Intent: "working contact form", Paths: []string{"/contact"}, Tools: []string{"manage_email"}},
		{ID: "auth", Title: "Admin auth", Intent: "login for the owner", Paths: []string{"/login"}, Tools: []string{"manage_auth", "manage_roles"}},
		{ID: "admin", Title: "Admin: manage projects", Intent: "owner-only project editor", Paths: []string{"/admin"}},
		{ID: "seo", Title: "SEO meta, sitemap, robots", Intent: "meta tags, sitemap and a feed", Tools: []string{"manage_seo", "manage_feeds"}},
	}}
)

// buildFloorBudget caps the BUILD static floor: system prompt plus serialized tool schemas,
// the bytes every single turn of a build re-reads whatever it is doing.
//
// The per-stage frame budgets in pack/promptsize_test.go do not cover this. They measure
// hand-authored prose only, with the catalogues and tool schemas collapsed, which is how the
// floor grew past 15K tokens without tripping anything. Raising this cap is the last resort:
// a floor of N tokens is paid on turn 0 in full, again on every rollover, and re-read at the
// provider's cache rate on every turn in between, so it is the most-multiplied number in the
// whole loop. Measured with the widest of the two plan shapes.
// Raised for the always-on window-bridge law in the module-scope contract, which
// rides the BUILD system prompt every turn (new guidance, single home).
// Raised for of_test.js user-flow testing (manage_testing write_flows/test_flows and the
// build rule that every interactive project ships one): new capability, single home.
// Raised for the per-action union schema (tools.reconcileUnionParam): each merged arg now
// names the actions that take it and keeps every action's distinct meaning, where it used to
// show only the first action's text and default (update-only semantics never reached the
// model). Partly paid back by compressing the manage_files/manage_testing action docs.
// Lowered from 82,000 when the build frame's project-specific rules and the REST/auth
// laws became conditional on the plan (pack/conditions.go): ratchet it down, not up.
// Raised by 400 for the build-close handoff rule (stop with a handoff, not a verdict):
// new always-on guidance with a single home, which removes the premature "all green"
// summary before the post-build checks run.
const buildFloorBudget = 77_400

// buildLoadedFloorBudget caps the floor for a REAL plan shape (loadedPlan), as opposed to
// the two minimal ones buildFloorBudget gates. It exists because the minimal plans resolve
// only a handful of tools while a realistic large plan resolves far more, so the real floor
// sits well above the minimal-plan gate. Set just above today's number: this is a ratchet on
// what actually ships, to be driven DOWN, and the lever is tool schemas (the largest
// component of the floor and the one carrying the least prose).
// Raised for of_test.js user-flow testing (manage_testing write_flows/test_flows and the
// build rule that every interactive project ships one): new capability, single home.
// Raised for the per-action union schema (see buildFloorBudget).
// Raised by 200 for the batched-question rule in the build stage prompt (ask everything in one turn, never poll).
// Lowered from 91,700 with the conditional build-frame rules (see buildFloorBudget).
// Raised by 300 for the manage_tasks block `needs_owner` flag (an owner's answer reopens
// only owner-blocked items) and the test_page sign-in note: both change what the model does.
// Raised by 300 for the build-close handoff rule (see buildFloorBudget).
const buildLoadedFloorBudget = 90_100

func TestBuildFloorBudget(t *testing.T) {
	cases := map[string]struct {
		plan   *Plan
		budget int
	}{
		"pages-only": {pagesOnlyPlan, buildFloorBudget},
		"with-table": {tablePlan, buildFloorBudget},
		"loaded":     {loadedPlan, buildLoadedFloorBudget},
	}
	for name, tc := range cases {
		sys, schemas, _ := buildFloorBytes(t, tc.plan)
		if got := len(sys) + len(schemas); got > tc.budget {
			t.Errorf("%s: BUILD static floor is %d bytes (~%d tokens), over its %d-byte budget by %d.\n"+
				"  Every turn of every build re-reads this. Find what grew (system prompt %d, tool schemas %d)\n"+
				"  and cut it; raising the cap is the last resort, not the fix.",
				name, got, got/4, tc.budget, got-tc.budget, len(sys), len(schemas))
		}
	}
}

// TestBackboneTierIsNotFreeForPagesOnlyPlans is the floor cost of the data-backbone tier,
// asserted rather than assumed: the tier is only worth preloading where the plan does data
// work, and this pins that dropping it actually removes bytes from the floor.
func TestBackboneTierIsNotFreeForPagesOnlyPlans(t *testing.T) {
	pagesSys, pagesSchemas, _ := buildFloorBytes(t, pagesOnlyPlan)
	tableSys, tableSchemas, _ := buildFloorBytes(t, tablePlan)

	pages := len(pagesSys) + len(pagesSchemas)
	table := len(tableSys) + len(tableSchemas)
	saved := table - pages
	if saved <= 0 {
		t.Fatalf("a pages-only plan floor (%d) is not below a table plan's (%d); the backbone tier "+
			"is being preloaded either way and the predicate is not doing anything", pages, table)
	}
	t.Logf("dropping the backbone tier saves %d bytes (~%d tokens) on every turn", saved, saved/4)
	// Both halves must move: the schemas leave the wire and the routing prose leaves the
	// system prompt, or the prompt would describe tools the model cannot call.
	if len(pagesSchemas) >= len(tableSchemas) {
		t.Errorf("tool schemas did not shrink: %d vs %d", len(pagesSchemas), len(tableSchemas))
	}
	if len(pagesSys) >= len(tableSys) {
		t.Errorf("system prompt did not shrink: routing prose for an unloaded tool would leave the "+
			"guidance and the wire schema disagreeing (%d vs %d)", len(pagesSys), len(tableSys))
	}
}

// perToolSchemaCap caps ONE tool's serialized wire schema.
//
// buildFloorBudget prices the floor as a lump, so one schema doubling hides behind
// another shrinking - which is how the preloaded block reached 20-32KB unnoticed. A
// per-tool cap prices each one on its own. It applies to every build-stage tool, not
// just the preloaded set: a plan that declares a tool preloads it, so any of them can
// land in the floor and be re-read on all 200 turns.
//
// Set a little above the largest today (manage_files). Over it, the fix is a tighter
// schema - fewer enum restatements, shorter per-field prose, situational detail moved
// to the tool's WhenToUse (which find_tool serves on demand) - not a bigger cap.
// Raised for of_test.js user-flow testing (manage_testing write_flows/test_flows and the
// build rule that every interactive project ships one): new capability, single home.
const perToolSchemaCap = 12_000

func TestPerToolSchemaSize(t *testing.T) {
	reg := tools.DefaultRegistry()
	names := []string{}
	for _, d := range reg.List() {
		names = append(names, d.Name)
	}
	defs := reg.LookupFull(names, llm.Capabilities{}, StageBuild)
	if len(defs) == 0 {
		t.Fatal("no build-stage tools resolved; the cap would pass vacuously")
	}
	largest, largestName := 0, ""
	for _, d := range defs {
		b, err := json.Marshal(d)
		if err != nil {
			t.Errorf("marshal %s: %v", d.Name, err)
			continue
		}
		if len(b) > largest {
			largest, largestName = len(b), d.Name
		}
		if len(b) > perToolSchemaCap {
			t.Errorf("tool %q serializes to %d bytes (cap %d) - a preloaded tool's schema is "+
				"re-read on every turn of every build. Tighten the schema rather than raising the cap.",
				d.Name, len(b), perToolSchemaCap)
		}
	}
	t.Logf("largest build-stage tool schema: %s at %d bytes (cap %d, across %d tools)",
		largestName, largest, perToolSchemaCap, len(defs))
}
