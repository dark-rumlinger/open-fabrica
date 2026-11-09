package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParsePlan_SlimShape pins the unified-loop plan shape: goal, voice, design,
// a task list, recall topics, and external libraries.
func TestParsePlan_SlimShape(t *testing.T) {
	raw := `{
		"goal": "A blog",
		"voice": "warm",
		"design": "clean, generous whitespace",
		"tasks": [
			{"id": "posts-table", "title": "Posts table + API", "intent": "create posts table"},
			{"title": "Home page", "deps": ["posts-table"], "paths": ["/files/home.js"]}
		],
		"recall_topics": ["canvas_rendering"],
		"external_libraries": [{"name": "marked", "url": "https://cdn.example.com/marked.js"}]
	}`
	plan, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("slim plan rejected: %v", err)
	}
	if plan.Goal != "A blog" || plan.Voice != "warm" || plan.Design == "" {
		t.Errorf("intent fields mis-parsed: %+v", plan)
	}
	if len(plan.Tasks) != 2 || plan.Tasks[0].ID != "posts-table" || plan.Tasks[1].Deps[0] != "posts-table" {
		t.Errorf("tasks mis-parsed: %+v", plan.Tasks)
	}
	if len(plan.ExternalLibraries) != 1 || plan.ExternalLibraries[0].Name != "marked" {
		t.Errorf("external_libraries mis-parsed: %+v", plan.ExternalLibraries)
	}
}

func TestPlanQuestion_BareStringShorthand(t *testing.T) {
	plan, err := ParsePlan(`{"questions":["What is your name?"]}`)
	if err != nil {
		t.Fatalf("bare-string shorthand rejected: %v", err)
	}
	if plan.Questions[0].Question != "What is your name?" || plan.Questions[0].Type != "text" {
		t.Errorf("bare-string shorthand mis-decoded: %+v", plan.Questions[0])
	}
}

func TestPlanQuestion_DefaultsTypeFromOptions(t *testing.T) {
	plan, err := ParsePlan(`{"questions":[{"question":"pick one","options":["A","B"]}]}`)
	if err != nil {
		t.Fatalf("ParsePlan failed: %v", err)
	}
	if plan.Questions[0].Type != "single_choice" {
		t.Errorf("expected single_choice, got %q", plan.Questions[0].Type)
	}
}

func TestParsePlan_DropsUnknownTopLevelKey(t *testing.T) {
	// Unknown top-level metadata the LLM invents is silently ignored.
	plan, err := ParsePlan(`{"goal":"x","version":"1.0","archetype":{"complexity":"large"}}`)
	if err != nil {
		t.Fatalf("expected unknown top-level keys to be ignored, got error: %v", err)
	}
	if plan.Goal != "x" {
		t.Errorf("expected goal preserved, got %q", plan.Goal)
	}
}

func TestParsePlan_DropsUnknownNestedKey(t *testing.T) {
	plan, err := ParsePlan(`{"questions":[{"question":"hi","unrelated_field":"oops"}]}`)
	if err != nil {
		t.Fatalf("expected unknown nested keys to be ignored, got error: %v", err)
	}
	if len(plan.Questions) != 1 || plan.Questions[0].Question != "hi" {
		t.Errorf("expected question preserved, got %+v", plan.Questions)
	}
}

func TestParsePlan_LibrarySpecRequiresObject(t *testing.T) {
	// Bare-string shorthand is rejected (json type mismatch); the canonical
	// {name, url} object parses cleanly.
	if _, err := ParsePlan(`{"external_libraries":["qrcode-generator"]}`); err == nil {
		t.Error("expected bare-string library to be rejected")
	}
	plan, err := ParsePlan(`{"external_libraries":[{"name":"qrcode","url":"https://cdn.example.com/qrcode.js"}]}`)
	if err != nil {
		t.Fatalf("full LibrarySpec form should parse: %v", err)
	}
	if len(plan.ExternalLibraries) != 1 || plan.ExternalLibraries[0].URL != "https://cdn.example.com/qrcode.js" {
		t.Errorf("URL not preserved: %+v", plan.ExternalLibraries)
	}
}

func TestParsePlan_StripsCodeFence(t *testing.T) {
	inner := `{"goal":"x","tasks":[{"title":"Home"}]}`
	for _, raw := range []string{
		"```json\n" + inner + "\n```",
		"```\n" + inner + "\n```",
		"```json\n" + inner + "\n```\n",
	} {
		if _, err := ParsePlan(raw); err != nil {
			t.Errorf("fenced plan should parse, got %v\ninput: %q", err, raw)
		}
	}
}

// TestParsePlan_FencedPlanBuriedInProse pins a planner failure mode: a reasoning
// model can narrate its analysis before the plan and append a "screen inventory
// check" after it, wrapping a fully valid plan in prose + a ```json
// fence. StripCodeFence only unwraps a fence around the WHOLE reply, so the
// first-byte guard rejected it ("expected object, got \"L\"") and forced a full
// plan re-generation. UnwrapJSON must recover the embedded block so it parses on
// the first attempt.
func TestParsePlan_FencedPlanBuriedInProse(t *testing.T) {
	raw := "Looking at this brief, I need to be careful about the security model.\n\n" +
		"Here's my plan:\n\n```json\n" +
		`{"goal":"A voxel sandbox","tasks":[{"title":"Game spine","done_when":"renders"}]}` +
		"\n```\n\n**Screen inventory check:** all named screens covered."
	plan, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("prose-wrapped fenced plan should parse, got %v", err)
	}
	if plan.Goal != "A voxel sandbox" || len(plan.Tasks) != 1 || plan.Tasks[0].Title != "Game spine" {
		t.Errorf("plan mis-parsed after unwrap: %+v", plan)
	}
}

func TestParsePlan_TrimsTrailingBraceOverrun(t *testing.T) {
	// Some local models close one brace too many; the parser trims the overrun.
	plan, err := ParsePlan(`{"goal":"x"}` + "\n\n}")
	if err != nil {
		t.Fatalf("trailing-brace overrun should be repaired, got %v", err)
	}
	if plan.Goal != "x" {
		t.Errorf("goal lost after brace trim: %q", plan.Goal)
	}
}

func TestParsePlan_EscapesRawControlChars(t *testing.T) {
	// A defect some models emit in a plan response: a literal
	// newline/tab inside a string value, which the JSON spec (and Go's decoder)
	// reject - forcing a full plan re-generation. The parser must repair it.
	raw := "{\"goal\":\"line one\nline two\"," +
		"\"design\":\"a\ttabbed\tdescription\"," +
		"\"tasks\":[{\"title\":\"Home\",\"intent\":\"do\nit\",\"done_when\":\"ok\"}]}"
	plan, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("raw control chars in string values should be repaired, got %v", err)
	}
	if !strings.Contains(plan.Goal, "line one") || !strings.Contains(plan.Goal, "line two") {
		t.Errorf("goal text not preserved across the repair: %q", plan.Goal)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].Title != "Home" {
		t.Fatalf("tasks not parsed after repair: %+v", plan.Tasks)
	}
}

// TestParsePlan_RepairsPythonLiteralsAndTrailingCommas pins a planner failure
// mode: some models emit a plan with Python-style True/False/None (Go's
// decoder rejects it with `invalid character 'T' ...`, forcing a full re-emit).
// The shared jsonrepair pass must salvage these - plus a trailing comma - so the
// plan parses on the first attempt.
func TestParsePlan_RepairsPythonLiteralsAndTrailingCommas(t *testing.T) {
	raw := `{"goal":"x","tasks":[{"title":"Home","done_when":"ok",}],"enabled":True,"disabled":False,"extra":None}`
	plan, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("Python literals / trailing comma should be repaired, got %v", err)
	}
	if plan.Goal != "x" || len(plan.Tasks) != 1 || plan.Tasks[0].Title != "Home" {
		t.Errorf("plan mis-parsed after repair: %+v", plan)
	}
}

func TestParsePlan_DedupesGuides(t *testing.T) {
	// Duplicates and blanks dropped; unknown slugs tolerated here (validated later
	// at prefetch against the real guide set).
	plan, err := ParsePlan(`{"guides":["webrtc_signaling"," ","webrtc_signaling","e2ee"]}`)
	if err != nil {
		t.Fatalf("ParsePlan failed: %v", err)
	}
	if len(plan.Guides) != 2 || plan.Guides[0] != "webrtc_signaling" || plan.Guides[1] != "e2ee" {
		t.Errorf("guides not deduped/trimmed: %v", plan.Guides)
	}
}

// TestParsePlanForStage_RejectsMangledTasksKey reproduces a failure mode that
// can ship an empty LIVE build: a weak model emits valid JSON under "taskss",
// which ParsePlan tolerantly drops, leaving zero tasks. parsePlanForStage must
// surface this as an error so the corrective-retry-then-fail path runs.
func TestParsePlanForStage_RejectsMangledTasksKey(t *testing.T) {
	_, err := parsePlanForStage(`{"goal":"x","taskss":[{"title":"t"}]}`)
	if err == nil {
		t.Fatal("expected an error for a plan whose tasks landed under a misspelled key")
	}
	// ParsePlan itself stays tolerant: it parses fine with zero tasks.
	if _, perr := ParsePlan(`{"goal":"x","taskss":[{"title":"t"}]}`); perr != nil {
		t.Errorf("ParsePlan should remain tolerant of unknown keys, got %v", perr)
	}
}

func TestParsePlanForStage_RejectsEmptyPlan(t *testing.T) {
	if _, err := parsePlanForStage(`{"goal":"x","tasks":[]}`); err == nil {
		t.Error("expected an error for a plan with no tasks and no questions")
	}
}

func TestParsePlanForStage_AllowsQuestionsOnly(t *testing.T) {
	// A questions-only plan is actionable: it pauses for owner input.
	if _, err := parsePlanForStage(`{"questions":["What is your name?"]}`); err != nil {
		t.Errorf("questions-only plan should be accepted, got %v", err)
	}
}

func TestParsePlanForStage_AllowsTask(t *testing.T) {
	if _, err := parsePlanForStage(`{"goal":"x","tasks":[{"title":"Home"}]}`); err != nil {
		t.Errorf("well-formed single-task plan should be accepted, got %v", err)
	}
}

// TestPlanExclusions_ObjectElements pins the tolerant decode: `exclusions` is a
// string array, and a model that emits richer object elements for it still gets
// a usable plan rather than a failed one. An object reduces to its label field;
// an object keyed by nothing recognised keeps its raw JSON so the planner's
// intent still reaches the build seed.
func TestPlanExclusions_ObjectElements(t *testing.T) {
	plan, err := ParsePlan(`{"tasks":[{"title":"t","intent":"i","done_when":"d"}],"exclusions":[
		{"item":"no auth","why":"out of scope"},
		{"feature":"leaderboard"},
		{"zzz":"unknown shape"}
	]}`)
	if err != nil {
		t.Fatalf("object-element exclusions rejected: %v", err)
	}
	want := []string{"no auth", "leaderboard", `{"zzz":"unknown shape"}`}
	if len(plan.Exclusions) != len(want) {
		t.Fatalf("exclusions = %#v, want %#v", []string(plan.Exclusions), want)
	}
	for i := range want {
		if plan.Exclusions[i] != want[i] {
			t.Errorf("exclusions[%d] = %q, want %q", i, plan.Exclusions[i], want[i])
		}
	}
}

// TestPlanExclusions_CanonicalFormUnaffected: the tolerant path must not change
// what a schema-clean plan decodes to, and must re-emit as a plain string array
// so a persisted plan round-trips through the canonical shape.
func TestPlanExclusions_CanonicalFormUnaffected(t *testing.T) {
	plan, err := ParsePlan(`{"tasks":[{"title":"t","intent":"i","done_when":"d"}],"exclusions":["no auth","no payments"]}`)
	if err != nil {
		t.Fatalf("canonical exclusions rejected: %v", err)
	}
	if len(plan.Exclusions) != 2 || plan.Exclusions[0] != "no auth" || plan.Exclusions[1] != "no payments" {
		t.Fatalf("canonical exclusions mis-parsed: %#v", []string(plan.Exclusions))
	}
	out, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"exclusions":["no auth","no payments"]`) {
		t.Errorf("exclusions did not re-emit as a string array: %s", out)
	}
}

// TestPlanExclusions_EmptyElementsDropped: an element carrying no text adds
// nothing to the out-of-scope line, so it never reaches it.
func TestPlanExclusions_EmptyElementsDropped(t *testing.T) {
	plan, err := ParsePlan(`{"tasks":[{"title":"t","intent":"i","done_when":"d"}],"exclusions":["a",null,"  ",{"item":""}]}`)
	if err != nil {
		t.Fatalf("ParsePlan failed: %v", err)
	}
	if len(plan.Exclusions) != 1 || plan.Exclusions[0] != "a" {
		t.Errorf("empty elements survived: %#v", []string(plan.Exclusions))
	}
}

// TestParsePlan_ErrorNamesSite pins that a decoder error says WHERE it failed.
// The message is both what a post-mortem reads and what the corrective retry
// feeds back, and a bare "invalid character 'n'" locates nothing in a plan of
// this size.
func TestParsePlan_ErrorNamesSite(t *testing.T) {
	// A dropped colon: valid up to the key, then a value where ':' was due.
	_, err := ParsePlan(`{"goal":"g","tasks":[{"title":"t","intent":"i","done_when":"d"}],"decisions":[{"topic":"x","choice":"y","check" null}]}`)
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "at byte ") {
		t.Errorf("error names no offset: %v", msg)
	}
	if !strings.Contains(msg, `\"check\" null`) {
		t.Errorf("error excerpt does not show the failure site: %v", msg)
	}
}

// The plan is re-marshalled into of_runs.plan_json, so a field the Plan struct
// does not carry is silently dropped on the way to its consumers. design_system
// must survive that round trip: the PWA manifest and the admin chat's token
// block both read it back out of the stored JSON.
func TestParsePlan_DesignSystemRoundTrips(t *testing.T) {
	raw := `{
	  "goal": "A maze game",
	  "design": "Neon wireframe on near-black.",
	  "design_system": {
	    "colors": {"primary": "#3b1f6e", "bg": "#050208", "accent": "#ff3df0"},
	    "font_display": "Space Grotesk",
	    "radius": "2px",
	    "motion": "180ms spring"
	  },
	  "tasks": [{"title": "Shell", "intent": "Layout + tokens", "done_when": "Tokens exist"}]
	}`
	plan, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if plan.DesignSystem == nil {
		t.Fatal("design_system must parse into the plan")
	}
	if plan.DesignSystem.Colors.Primary != "#3b1f6e" || plan.DesignSystem.FontDisplay != "Space Grotesk" {
		t.Fatalf("design_system fields lost: %+v", plan.DesignSystem)
	}

	// The round-trip through JSON must preserve it.
	stored, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		DesignSystem struct {
			Colors struct {
				Primary string `json:"primary"`
				Bg      string `json:"bg"`
			} `json:"colors"`
		} `json:"design_system"`
	}
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatalf("unmarshal stored plan: %v", err)
	}
	if back.DesignSystem.Colors.Primary != "#3b1f6e" || back.DesignSystem.Colors.Bg != "#050208" {
		t.Fatalf("design_system did not survive the round trip: %s", stored)
	}
}

// A plan with no design_system must not emit an empty object: the PWA falls back
// to its own defaults, and an empty block would suppress that.
func TestParsePlan_DesignSystemOmittedWhenAbsent(t *testing.T) {
	plan, err := ParsePlan(`{"goal":"x","tasks":[{"title":"T","intent":"i","done_when":"d"}]}`)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	stored, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(stored), "design_system") {
		t.Fatalf("absent design_system must be omitted, got: %s", stored)
	}
}

// A planner that does not honour the schema can still emit an empty shell. It
// must not reach plan_json, where it would render as an empty tokens block.
func TestParsePlan_DropsEmptyDesignSystem(t *testing.T) {
	for _, raw := range []string{
		`{"design_system":{"colors":{}},"tasks":[{"title":"T","intent":"i","done_when":"d"}]}`,
		`{"design_system":{"colors":{"primary":"  "},"motion":""},"tasks":[{"title":"T","intent":"i","done_when":"d"}]}`,
	} {
		plan, err := ParsePlan(raw)
		if err != nil {
			t.Fatalf("ParsePlan: %v", err)
		}
		if plan.DesignSystem != nil {
			t.Fatalf("an all-empty design_system must be dropped, got %+v", plan.DesignSystem)
		}
		stored, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(stored), "design_system") {
			t.Fatalf("empty design_system must not serialize: %s", stored)
		}
	}
}

// Planners annotate colour values. The manifest writes colours through verbatim,
// so the hex is kept and the prose dropped.
func TestParsePlan_TrimsAnnotatedColours(t *testing.T) {
	plan, err := ParsePlan(`{"design_system":{"colors":{
		"primary":"#d8b4b8 (rose quartz - hover, copy button)",
		"bg":"#050208",
		"surface":"  #FFFDF7  ",
		"text":"rgb(20,20,20)",
		"accent":"#abc"}},
		"tasks":[{"title":"T","intent":"i","done_when":"d"}]}`)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	c := plan.DesignSystem.Colors
	for _, tc := range []struct{ got, want, name string }{
		{c.Primary, "#d8b4b8", "annotated hex is trimmed to the literal"},
		{c.Bg, "#050208", "a bare hex is untouched"},
		{c.Surface, "#FFFDF7", "surrounding space is trimmed"},
		{c.Text, "rgb(20,20,20)", "a non-hex value is left as written"},
		{c.Accent, "#abc", "short hex is kept"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// A task's id lists survive the shape drift a model emits when the schema is not
// enforced: "tools":[[]] (seen on GLM), a lone string, a nested list. One stray bracket
// must not fail the plan and force a full re-emit.
func TestParsePlanToleratesIDListDrift(t *testing.T) {
	raw := `{"goal":"g","tasks":[
		{"id":"a","title":"A","paths":["contact_messages"],"tools":[[]],"exports":[]},
		{"id":"b","title":"B","tools":"manage_seo","deps":[["a"],"a2"],"covers":[1,"f1",null]}
	]}`
	p, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("plan with id-list drift must parse: %v", err)
	}
	a, b := p.Tasks[0], p.Tasks[1]
	if len(a.Tools) != 0 || len(a.Paths) != 1 || a.Paths[0] != "contact_messages" {
		t.Errorf("task a: tools=%v paths=%v", a.Tools, a.Paths)
	}
	if len(b.Tools) != 1 || b.Tools[0] != "manage_seo" {
		t.Errorf("a lone string becomes a one-item list, got %v", b.Tools)
	}
	if strings.Join(b.Deps, ",") != "a,a2" || strings.Join(b.Covers, ",") != "f1" {
		t.Errorf("nested strings are kept and non-strings dropped: deps=%v covers=%v", b.Deps, b.Covers)
	}
}
