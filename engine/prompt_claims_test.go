package engine

// Claim guard: the rendered tool-bearing system prompts make factual claims about the
// session they ride - which tools are already loaded, what the catalogue lines carry,
// which sections find_tool's description points at - and every one of those claims
// has drifted from the code at least once (a catalogue preamble promising action verbs
// the lines never carried; scheduled runs advertising tools they never preloaded).
// The name-existence guards cannot see that class: every name was real, the claim
// about it was false. These tests render the REAL prompt for each tool-bearing stage
// and check each claim against the session's actual tool set.

import (
	"regexp"
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// promptSection returns the body of a `## <heading>` section: everything after the
// heading line up to the next `## ` heading or the end.
func promptSection(sys, heading string) (string, bool) {
	i := strings.Index(sys, "\n"+heading+"\n")
	if i < 0 {
		return "", false
	}
	body := sys[i+len(heading)+2:]
	if j := strings.Index(body, "\n## "); j >= 0 {
		body = body[:j]
	}
	return body, true
}

var (
	routedToolRe  = regexp.MustCompile("(?m)^### `([a-z_]+)`")
	catalogToolRe = regexp.MustCompile("(?m)^- `([a-z_]+)`:")
)

// checkToolClaims holds one rendered prompt to the tool set its session really loads:
// the always-loaded block names only preloaded tools and covers every tool in
// alwaysBlock (the set that block is responsible for - BUILD's plan-declared extras
// get their prose in the seed instead); the catalogue names only tools that are NOT
// preloaded (it advertises what find_tool can add); and the catalogue preamble promises
// action verbs only if the lines carry them. preloaded is resolved the way the session
// resolves it (stage- and capability-gated), so a name the stage drops never counts
// as loaded.
func checkToolClaims(t *testing.T, stage, sys, catalogueHeading string, preloaded, alwaysBlock []string) {
	t.Helper()
	var caps llm.Capabilities
	loaded := map[string]bool{}
	for _, d := range tools.DefaultRegistry().LookupFull(preloaded, caps, stageOf(stage)) {
		loaded[d.Name] = true
	}

	always, ok := promptSection(sys, "## Your always-loaded tools")
	if !ok {
		t.Fatalf("%s prompt has no always-loaded tools section", stage)
	}
	routed := map[string]bool{}
	for _, m := range routedToolRe.FindAllStringSubmatch(always, -1) {
		routed[m[1]] = true
		if !loaded[m[1]] {
			t.Errorf("%s prompt advertises %q as always-loaded, but the session does not preload it - the model is told not to find_tool it and every call comes back unloaded", stage, m[1])
		}
	}
	for _, n := range alwaysBlock {
		if !loaded[n] {
			continue
		}
		if td, err := tools.DefaultRegistry().Get(n); err == nil && strings.TrimSpace(td.WhenToUse) != "" && !routed[n] {
			t.Errorf("%s session preloads %q but its routing prose is missing from the always-loaded section", stage, n)
		}
	}

	catalogue, ok := promptSection(sys, catalogueHeading)
	if !ok {
		t.Fatalf("%s prompt has no %q section, which find_tool's description points at", stage, catalogueHeading)
	}
	listed := catalogToolRe.FindAllStringSubmatch(catalogue, -1)
	if len(listed) == 0 {
		t.Fatalf("%s catalogue lists no tools", stage)
	}
	for _, m := range listed {
		if loaded[m[1]] {
			t.Errorf("%s catalogue lists %q as loadable, but it is already preloaded", stage, m[1])
		}
	}
	claimsVerbs := strings.Contains(catalogue, "exact `action` verbs") || strings.Contains(catalogue, "listed verbs")
	if claimsVerbs && !strings.Contains(catalogue, "actions:") {
		t.Errorf("%s catalogue preamble promises action verbs, but no catalogue line carries any", stage)
	}
}

// find_tool's description names the section each stage's loadable tools live under.
// If a heading is renamed, the description points at nothing.
func TestFindToolDescriptionNamesRealSections(t *testing.T) {
	desc := tools.FindToolDef().Description
	for _, heading := range []string{"## Tool catalogue", "## Deferred capabilities"} {
		if !strings.Contains(desc, heading) {
			t.Errorf("find_tool's description no longer names %q; update this guard with it", heading)
		}
	}
	if !strings.Contains(pack.ToolCataloguePreamble, "stays available for the rest of the run") {
		t.Skip("the preamble no longer claims loads persist; TestFindToolLoadSurvivesRollover guards that claim")
	}
}

// BUILD: the always-loaded block and the deferred index against buildAllTools.
func TestBuildPromptToolClaimsMatchPreloads(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	for name, plan := range map[string]*Plan{
		"empty plan": {},
		"data plan": {Tasks: []PlanTaskSpec{
			{ID: "posts", Title: "Posts table", Paths: []string{"posts"}},
			{Title: "Home", Paths: []string{"/"}, Deps: []string{"posts"}},
		}},
		"pages only": {Tasks: []PlanTaskSpec{{Title: "Home", Paths: []string{"/"}}}},
		"realtime plan": {Tasks: []PlanTaskSpec{
			{Title: "Chat relay", Paths: []string{"chat"}, Tools: []string{"manage_relay"}},
			{Title: "Home", Paths: []string{"/"}},
		}},
	} {
		sys := w.buildSystemPrompt(plan)
		// The always-loaded block carries the core (+ backbone); plan extras ride the seed
		// (TestBuildSharedSeedCarriesPlanToolRoutingProse), but must still be off the catalogue.
		always := append([]string{}, tools.CoreBuildToolNames...)
		if planNeedsBackbone(plan) {
			always = append(always, tools.BuildBackboneToolNames...)
		}
		checkToolClaims(t, "BUILD/"+name, sys, "## Deferred capabilities", buildAllTools(plan), always)
	}
}

// LIVE and scheduled runs share one system prompt; both sessions must load what it
// advertises.
func TestLivePromptToolClaimsMatchPreloads(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	sys := w.liveSystemPrompt()
	live := w.liveWakeSpec("hi").PreloadTools
	checkToolClaims(t, "LIVE", sys, "## Tool catalogue", live, live)
	sched := w.scheduledTaskSpec("digest").PreloadTools
	checkToolClaims(t, "scheduled", sys, "## Tool catalogue", sched, sched)
}

// stageOf maps a checkToolClaims label ("BUILD/...", "LIVE", "scheduled") to the
// stage whose gating the session applies.
func stageOf(label string) string {
	if strings.HasPrefix(label, "BUILD") {
		return StageBuild
	}
	return StageLive
}

// The ground rules name the preloaded tools per stage in prose. Hold that prose to the
// real sets: "plus manage_tasks in BUILD, and manage_codebase / manage_diagnostics in
// LIVE" must stay true of buildPreloadTools and livePreloadTools.
func TestGroundRulesPreloadClaimsAreTrue(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	var rules string
	for _, p := range ps {
		if p.Slug == "_common_rules" {
			rules = p.Body
		}
	}
	if rules == "" {
		t.Fatal("_common_rules fragment not found")
	}
	var caps llm.Capabilities
	available := func(stage string, names []string) map[string]bool {
		out := map[string]bool{}
		for _, d := range tools.DefaultRegistry().LookupFull(names, caps, stage) {
			out[d.Name] = true
		}
		return out
	}
	build := available(StageBuild, buildPreloadTools(Task{}, false))
	live := available(StageLive, livePreloadTools())
	for _, n := range []string{"manage_files", "manage_testing", "manage_guides", "manage_tasks"} {
		if strings.Contains(rules, "`"+n+"`") && !build[n] {
			t.Errorf("ground rules call %q preloaded, but BUILD does not preload it", n)
		}
	}
	for _, n := range []string{"manage_codebase", "manage_diagnostics", "manage_files", "manage_testing", "manage_guides"} {
		if strings.Contains(rules, n) && !live[n] {
			t.Errorf("ground rules call %q preloaded in LIVE, but LIVE does not preload it", n)
		}
	}
	if live["manage_tasks"] {
		t.Error("the ground rules scope manage_tasks to BUILD, but LIVE loads it")
	}
}
