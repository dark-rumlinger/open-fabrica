package project

import (
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/tools"
)

// manage_guides recall returns one guide's full body. The session truncates a
// tool result to the tool's MaxResult (and to a small default when unset), so a
// guide body larger than MaxResult reaches the model as a fragment - and recall
// has no offset arg, so it cannot page in the rest. The big reference guides
// (e2ee, webrtc, auth) are exactly the ones a build/live fix must author from,
// so MaxResult must clear the largest shipped guide body plus its JSON envelope.
// If a future guide grows past the budget, this fails and prompts a bump rather
// than silently shipping a truncated contract.
func TestManageGuidesMaxResultFitsLargestGuide(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("load packs: %v", err)
	}

	// recall serves the TRANSCLUDED body (contract tokens expanded), so measure
	// that - it is what actually reaches the model.
	largest, slug := 0, ""
	for _, p := range ps {
		if p.Role != pack.RoleCapability {
			continue
		}
		if n := len(pack.TranscludeContracts(p.Body)); n > largest {
			largest, slug = n, p.Slug
		}
	}
	if largest == 0 {
		t.Fatal("no capability packs found")
	}

	def, err := tools.DefaultRegistry().Get("manage_guides")
	if err != nil {
		t.Fatalf("manage_guides not registered: %v", err)
	}

	// recall wraps the body in a JSON envelope and escapes it; require headroom
	// over the raw body so the contract is never delivered truncated.
	need := int(float64(largest)*1.15) + 1024
	if def.MaxResult < need {
		t.Fatalf("manage_guides MaxResult=%d is too small for the largest guide %q (%d bytes, ~%d with JSON envelope) - recall would truncate it. Raise MaxResult.",
			def.MaxResult, slug, largest, need)
	}
}

// TestGuideRecallToleratesNearMissSlugs pins the recall hardening: the exact
// slug still resolves, and the near-miss forms a model actually produces
// (whitespace, wrong case, hyphens for underscores) resolve to the same guide
// instead of dead-ending. A genuine miss must return suggestions or point at
// list/search - never a bare "guide not found".
func TestGuideRecallToleratesNearMissSlugs(t *testing.T) {
	ctx := &tools.ToolContext{}

	for _, slug := range []string{
		"realtime_signaling",
		"  realtime_signaling  ",
		"Realtime_Signaling",
		"realtime-signaling",
	} {
		res, err := (&GuideRecall{Slug: slug}).Run(ctx)
		if err != nil {
			t.Fatalf("recall %q: %v", slug, err)
		}
		if !res.Success {
			t.Errorf("recall %q should resolve, got: %s", slug, res.Error)
			continue
		}
		data, ok := res.Data.(map[string]any)
		if !ok || data["slug"] != "realtime_signaling" {
			t.Errorf("recall %q resolved to %v, want realtime_signaling", slug, data["slug"])
		}
	}

	// A near-name miss fails with a did-you-mean pointing at the real slug.
	res, err := (&GuideRecall{Slug: "realtime"}).Run(ctx)
	if err != nil {
		t.Fatalf("recall miss: %v", err)
	}
	if res.Success {
		t.Fatal("a partial slug must not silently resolve")
	}
	if !strings.Contains(res.Error, "realtime_signaling") {
		t.Errorf("miss error should suggest realtime_signaling, got: %s", res.Error)
	}

	// A total miss still tells the model how to recover.
	res, err = (&GuideRecall{Slug: "zzz_nonexistent"}).Run(ctx)
	if err != nil {
		t.Fatalf("recall total miss: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "list") {
		t.Errorf("total-miss error should point at action=list, got: %s", res.Error)
	}
}

// TestGuideRecallSkipsSeedInlinedCore: when the session seed already inlines a
// guide's core (ToolContext.SeedGuides), recall serves the section index or the
// extended sections instead of a near-duplicate of text the model holds.
func TestGuideRecallSkipsSeedInlinedCore(t *testing.T) {
	seeded := &tools.ToolContext{SeedGuides: map[string]bool{"realtime_signaling": true}}

	res, err := (&GuideRecall{Slug: "realtime_signaling"}).Run(seeded)
	if err != nil || !res.Success {
		t.Fatalf("seeded recall: err=%v res=%+v", err, res)
	}
	data := res.Data.(map[string]any)
	if _, has := data["body"]; has {
		t.Error("recall of a seed-inlined guide should omit the core body")
	}
	if note, _ := data["note"].(string); !strings.Contains(note, "already inlined") {
		t.Errorf("note should say the core is already in context, got: %q", note)
	}
	if _, has := data["sections"]; !has {
		t.Error("the section index should still be served")
	}

	// section=all serves the extended half only, strictly smaller than the
	// full body an un-seeded recall returns.
	res, err = (&GuideRecall{Slug: "realtime_signaling", Section: "all"}).Run(seeded)
	if err != nil || !res.Success {
		t.Fatalf("seeded section=all: err=%v res=%+v", err, res)
	}
	extBody, _ := res.Data.(map[string]any)["body"].(string)
	if extBody == "" {
		t.Fatal("seeded section=all should serve the extended sections")
	}

	res, err = (&GuideRecall{Slug: "realtime_signaling", Section: "all"}).Run(&tools.ToolContext{})
	if err != nil || !res.Success {
		t.Fatalf("un-seeded section=all: err=%v res=%+v", err, res)
	}
	fullBody, _ := res.Data.(map[string]any)["body"].(string)
	if len(extBody) >= len(fullBody) {
		t.Errorf("extended-only body (%d bytes) should be smaller than the full body (%d bytes)", len(extBody), len(fullBody))
	}

	// Without SeedGuides the default recall still serves the core.
	res, err = (&GuideRecall{Slug: "realtime_signaling"}).Run(&tools.ToolContext{})
	if err != nil || !res.Success {
		t.Fatalf("un-seeded recall: err=%v res=%+v", err, res)
	}
	if _, has := res.Data.(map[string]any)["body"]; !has {
		t.Error("un-seeded recall should serve the core body")
	}
}

// TestGuideRecallReturnsRelated pins the related-guides pointer: recalling a
// guide with requires/category links surfaces their slugs so an "owned by
// <other guide>" cross-reference is followable in one more call.
func TestGuideRecallReturnsRelated(t *testing.T) {
	ctx := &tools.ToolContext{}

	// 3d_rendering and game_feel are same-category (rendering) siblings with no
	// requires link between them, so surfacing one from the other exercises the
	// category-sibling path specifically.
	res, err := (&GuideRecall{Slug: "3d_rendering"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("recall 3d_rendering: err=%v res=%+v", err, res)
	}
	data := res.Data.(map[string]any)
	rel, ok := data["related"].([]map[string]string)
	if !ok {
		t.Fatalf("related missing or wrong shape: %T", data["related"])
	}
	found := false
	for _, r := range rel {
		if r["slug"] == "game_feel" {
			found = true
			if r["summary"] == "" {
				t.Error("related entry should carry the summary one-liner")
			}
		}
		if r["slug"] == "3d_rendering" {
			t.Error("related must not include the recalled guide itself")
		}
	}
	if !found {
		t.Errorf("3d_rendering related should include its rendering sibling game_feel, got %v", rel)
	}

	// A requires base is surfaced too (e2ee requires realtime_signaling).
	res, err = (&GuideRecall{Slug: "e2ee"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("recall e2ee: err=%v res=%+v", err, res)
	}
	rel = res.Data.(map[string]any)["related"].([]map[string]string)
	found = false
	for _, r := range rel {
		if r["slug"] == "realtime_signaling" {
			found = true
		}
	}
	if !found {
		t.Errorf("e2ee related should include its requires base realtime_signaling, got %v", rel)
	}
}

// TestGuidesHiddenForDeadCapability: a guide the session cannot back with a
// provider is absent from list and search, and recall says why instead of
// serving it.
func TestGuidesHiddenForDeadCapability(t *testing.T) {
	ctx := &tools.ToolContext{HiddenGuides: map[string]bool{"media_generation": true}}

	res, err := (&GuideList{}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("list: err=%v res=%+v", err, res)
	}
	for _, row := range res.Data.(map[string]any)["guides"].([]map[string]any) {
		if row["slug"] == "media_generation" {
			t.Error("list still names a hidden guide")
		}
	}

	res, err = (&GuideSearch{Query: "media_generation"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("search: err=%v res=%+v", err, res)
	}
	for _, row := range res.Data.(map[string]any)["guides"].([]map[string]any) {
		if row["slug"] == "media_generation" {
			t.Error("search still returns a hidden guide")
		}
	}

	res, err = (&GuideRecall{Slug: "media_generation"}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !strings.Contains(res.Error, "unavailable") {
		t.Errorf("recall of a hidden guide should explain it is unavailable, got %+v", res)
	}
}

// TestGuideRecallUnsplitInlinedHasNoDeadEnd: an inlined guide with no extended
// sections must not tell the model to pull a section that does not exist.
func TestGuideRecallUnsplitInlinedHasNoDeadEnd(t *testing.T) {
	gs, err := capabilityPacks(nil)
	if err != nil {
		t.Fatal(err)
	}
	var slug string
	for _, g := range gs {
		if len(g.Sections) == 0 {
			slug = g.Slug
			break
		}
	}
	if slug == "" {
		t.Skip("every guide is split")
	}
	ctx := &tools.ToolContext{SeedGuides: map[string]bool{slug: true}}
	res, err := (&GuideRecall{Slug: slug}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("recall: err=%v res=%+v", err, res)
	}
	note, _ := res.Data.(map[string]any)["note"].(string)
	if strings.Contains(note, "Pull a section") || !strings.Contains(note, "nothing more to pull") {
		t.Errorf("note should say there is nothing more to pull, got %q", note)
	}
}

// TestGuideSearchMatchesAcrossTerms: a multi-word query matches a guide that
// holds every word, and a truncated result says so.
func TestGuideSearchMatchesAcrossTerms(t *testing.T) {
	res, err := (&GuideSearch{Query: "stripe webhook"}).Run(&tools.ToolContext{})
	if err != nil || !res.Success {
		t.Fatalf("search: err=%v res=%+v", err, res)
	}
	rows := res.Data.(map[string]any)["guides"].([]map[string]any)
	found := false
	for _, r := range rows {
		if r["slug"] == "payments" {
			found = true
		}
	}
	if !found {
		t.Errorf("a two-word query should reach the payments guide, got %v", rows)
	}

	res, _ = (&GuideSearch{Query: ""}).Run(&tools.ToolContext{})
	data := res.Data.(map[string]any)
	if data["truncated"] != true {
		t.Errorf("an empty query matches every guide, so it must report truncation: %v", data)
	}
}
