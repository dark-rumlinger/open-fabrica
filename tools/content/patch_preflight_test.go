package content

import (
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The defects these pin: hunks apply IN ORDER, each against the result of the
// previous one, so two hunks aimed at the same single occurrence can never both
// land - the post-apply consumed diagnosis owns that case. Array-shape defects
// (an empty search, a hunk with no replace key) are rejected before any mutation.

// Two hunks sharing an anchor that occurs ONCE: the first consumes it, the second
// misses, and the atomic apply reports the consumed diagnosis with nothing saved.
func TestPatch_SameAnchorOneOccurrenceFailsAsConsumed(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "const cfg = { a: 1 };\nfunction step(){ return cfg.a; }\nfunction update(){ return 0; }\nfunction render(){ return 1; }\nfunction frame() {\n  step();\n  render();\n}\nexport function boot() {\n  frame();\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/world.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	frame := "function frame() {\n  step();\n  render();\n}"
	res, _ := artifactActionPatch(ctx, "file", "js/world.js", []map[string]any{
		{"search": "const cfg = { a: 1 };", "replace": "const cfg = { a: 2 };"},
		{"search": frame, "replace": "two pseudo lines that won't match anyway"},
		{"search": frame, "replace": "function frame() {\n  step();\n  update();\n  render();\n}"},
		{"search": "  frame();", "replace": "  frame();\n  render();"},
	})
	if res == nil || res.Success {
		t.Fatalf("two hunks over one occurrence must fail atomically, got %+v", res)
	}
	if res.Code != "patch_anchor_consumed" {
		t.Fatalf("Code = %q, want patch_anchor_consumed", res.Code)
	}
	// This is NOT anchor drift, so that steer may not leak in.
	for _, banned := range []string{"get the exact region", "nearest_current_regions", "re-save the whole corrected body"} {
		if contains(res.Error, banned) {
			t.Errorf("reject must not use the anchor-drift steer %q, got: %s", banned, res.Error)
		}
	}

	a, _ := store.ArtifactGet(db, "file", "js/world.js")
	if a == nil || a.Body != body {
		t.Fatalf("an atomic reject must leave the file byte-identical, got %+v", a)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "js/world.js"); v != 1 {
		t.Fatalf("an atomic reject must not bump the version, got %d", v)
	}
}

// Same anchor, distinct `near`, both occurrences change.
func TestPatch_DuplicateAnchorWithDistinctNearApplies(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function up() {\n  count++;\n}\nfunction down() {\n  count++;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/counter.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/counter.js", []map[string]any{
		{"search": "  count++;", "replace": "  count += 2;", "near": "function up()"},
		{"search": "  count++;", "replace": "  count -= 1;", "near": "function down()"},
	})
	if res == nil || !res.Success {
		t.Fatalf("distinct near must apply both hunks, got %+v", res)
	}
	a, _ := store.ArtifactGet(db, "file", "js/counter.js")
	if !contains(a.Body, "count += 2;") || !contains(a.Body, "count -= 1;") {
		t.Fatalf("both occurrences should have changed, got %q", a.Body)
	}
}

// Same anchor, TWO occurrences in the body, default counts: each hunk consumes the
// first remaining occurrence, so both land. Rejecting this pushed callers to a full
// re-save of a file they held correctly.
func TestPatch_DuplicateAnchorWithEnoughOccurrencesApplies(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function up() {\n  count++;\n}\nfunction down() {\n  count++;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/twice.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/twice.js", []map[string]any{
		{"search": "  count++;", "replace": "  count += 2;"},
		{"search": "  count++;", "replace": "  count -= 1;"},
	})
	if res == nil || !res.Success {
		t.Fatalf("two hunks over two occurrences must both apply, got %+v", res)
	}
	a, _ := store.ArtifactGet(db, "file", "js/twice.js")
	if !contains(a.Body, "count += 2;") || !contains(a.Body, "count -= 1;") {
		t.Fatalf("both occurrences should have changed in order, got %q", a.Body)
	}
}

// Regression pin for the old pre-flight false positive: a same-anchor pair whose
// anchors only match through the tolerant layers (CRLF vs the stored LF) used to
// be rejected as a duplicate; it must simply apply.
func TestPatch_DriftedSameAnchorPairApplies(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function up() {\n  count++;\n}\nfunction down() {\n  count++;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/crlf.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/crlf.js", []map[string]any{
		{"search": "  count++;\r\n}", "replace": "  count += 2;\r\n}"},
		{"search": "  count++;\r\n}", "replace": "  count -= 1;\r\n}"},
	})
	if res == nil || !res.Success {
		t.Fatalf("drifted same-anchor pair must apply through the tolerant layers, got %+v", res)
	}
	a, _ := store.ArtifactGet(db, "file", "js/crlf.js")
	if !contains(a.Body, "count += 2;") || !contains(a.Body, "count -= 1;") {
		t.Fatalf("both occurrences should have changed, got %q", a.Body)
	}
}

// A hunk keyed with an UNRECOGNIZED field decodes to an empty search. Without this reject
// ApplyPatches skips it silently, so a batch of nothing but mis-keyed hunks lands as SUCCESS
// having changed nothing and still bumped the version. (The common dialect aliases like
// old_string are absorbed before this point; see TestPatch_AliasKeysAccepted.)
func TestPatch_EmptySearchRejected(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "const x = 1;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/c.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/c.js", []map[string]any{
		{"needle": "const x = 1;", "replace": "const x = 2;"},
	})
	if res == nil || res.Success {
		t.Fatalf("an empty search must be refused, not silently dropped, got %+v", res)
	}
	if res.Code != "patch_empty_search" {
		t.Fatalf("Code = %q, want patch_empty_search", res.Code)
	}
	a, _ := store.ArtifactGet(db, "file", "js/c.js")
	if a == nil || a.Body != body {
		t.Fatalf("file must be unchanged, got %+v", a)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "js/c.js"); v != 1 {
		t.Fatalf("an empty-search batch must not bump the version, got %d", v)
	}
}

// The common patch dialects other editors use (Anthropic's Edit old_string/new_string,
// plain find/replace, from/to) are aliased to search/replace and apply cleanly instead of
// bouncing off patch_empty_search. The pairs matter together: old_string without new_string
// would flip to a missing replace, so both sides must land.
func TestPatch_AliasKeysAccepted(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	cases := []struct {
		name string
		hunk map[string]any
	}{
		{"old_new_string", map[string]any{"old_string": "const x = 1;", "new_string": "const x = 2;"}},
		{"find_replace", map[string]any{"find": "const x = 1;", "replace": "const x = 2;"}},
		{"from_to", map[string]any{"from": "const x = 1;", "to": "const x = 2;"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "const x = 1;\n"
			if res, _ := artifactActionSaveP(ctx, "file", "js/"+tc.name+".js", body, "", "", nil, "", false); !res.Success {
				t.Fatalf("seed save failed: %q", res.Error)
			}
			res, _ := artifactActionPatch(ctx, "file", "js/"+tc.name+".js", []map[string]any{tc.hunk})
			if res == nil || !res.Success {
				t.Fatalf("alias hunk must apply, got %+v", res)
			}
			a, _ := store.ArtifactGet(db, "file", "js/"+tc.name+".js")
			if a == nil || a.Body != "const x = 2;\n" {
				t.Fatalf("alias hunk must edit the body, got %+v", a)
			}
		})
	}
}

// A present canonical key wins over an alias, and an alias never overwrites real search text.
func TestPatch_CanonicalWinsOverAlias(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "const x = 1;\nconst y = 3;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/win.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	// search is set; old_string points elsewhere and must be ignored.
	res, _ := artifactActionPatch(ctx, "file", "js/win.js", []map[string]any{
		{"search": "const y = 3;", "old_string": "const x = 1;", "replace": "const y = 4;"},
	})
	if res == nil || !res.Success {
		t.Fatalf("patch must apply against the canonical search, got %+v", res)
	}
	a, _ := store.ArtifactGet(db, "file", "js/win.js")
	if a == nil || a.Body != "const x = 1;\nconst y = 4;\n" {
		t.Fatalf("canonical search must win over the alias, got %+v", a)
	}
}

// A hunk with a search but no replace key would silently DELETE the span - the
// mirror of the empty-search mistake, refused just as loudly.
func TestPatch_MissingReplaceRejected(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "const x = 1;\nconst y = 2;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/norep.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/norep.js", []map[string]any{
		{"search": "const x = 1;"},
	})
	if res == nil || res.Success {
		t.Fatalf("a hunk with no replace key must be refused, got %+v", res)
	}
	if res.Code != "patch_missing_replace" {
		t.Fatalf("Code = %q, want patch_missing_replace", res.Code)
	}
	a, _ := store.ArtifactGet(db, "file", "js/norep.js")
	if a == nil || a.Body != body {
		t.Fatalf("file must be unchanged, got %+v", a)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "js/norep.js"); v != 1 {
		t.Fatalf("a missing-replace batch must not bump the version, got %d", v)
	}
}

// An explicit empty replace is a deliberate deletion and still applies.
func TestPatch_ExplicitEmptyReplaceDeletes(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "const x = 1;\nconst y = 2;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/del.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/del.js", []map[string]any{
		{"search": "const x = 1;\n", "replace": ""},
	})
	if res == nil || !res.Success {
		t.Fatalf("an explicit empty replace must delete the span, got %+v", res)
	}
	a, _ := store.ArtifactGet(db, "file", "js/del.js")
	if a == nil || contains(a.Body, "const x = 1;") {
		t.Fatalf("span should have been deleted, got %+v", a)
	}
}

// A single drifted no-op hunk (search == replace, anchor present only through the
// tolerant layers) is a no-op, never "consumed by an earlier hunk" - there is none.
func TestPatch_DriftedNoOpIsNoOpNotConsumed(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function up() {\n  count++;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/noop.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	anchor := "  count++;\r\n}"
	res, _ := artifactActionPatch(ctx, "file", "js/noop.js", []map[string]any{
		{"search": anchor, "replace": anchor},
	})
	if res == nil || res.Success {
		t.Fatalf("a no-op hunk must fail, got %+v", res)
	}
	if res.Code != "patch_noop_replace" {
		t.Fatalf("Code = %q, want patch_noop_replace", res.Code)
	}
	if contains(res.Error, "EARLIER hunk") {
		t.Errorf("a single-hunk no-op must not claim an earlier hunk consumed it, got: %s", res.Error)
	}
}

// A deletion located only by the fuzzy matcher is refused under its own code, and
// the message must not claim the text is absent - it WAS located.
func TestPatch_FuzzyDeletionDistinctCode(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function compute(a, b) {\n  const total = a + b;\n  return total;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/fuzzy.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/fuzzy.js", []map[string]any{
		{"search": "function compute(a, b) {\n  const total = a + c;\n  return total;\n}", "replace": ""},
	})
	if res == nil || res.Success {
		t.Fatalf("a fuzzy-located deletion must be refused, got %+v", res)
	}
	if res.Code != "patch_fuzzy_deletion" {
		t.Fatalf("Code = %q, want patch_fuzzy_deletion", res.Code)
	}
	if !contains(res.Error, "approximate") {
		t.Errorf("reject must explain the approximate-match refusal, got: %s", res.Error)
	}
	if contains(res.Error, "not in the current file") {
		t.Errorf("reject must not claim the text is absent, got: %s", res.Error)
	}
	a, _ := store.ArtifactGet(db, "file", "js/fuzzy.js")
	if a == nil || a.Body != body {
		t.Fatalf("a refused deletion must leave the file unchanged, got %+v", a)
	}
}

// The overlap case pre-flight cannot see: hunk B's anchor is a distinct substring that
// hunk A's replacement swallows. The anchor was correct when sent, so the reject must
// say so and steer AWAY from a re-read.
func TestPatch_ConsumedAnchorDiagnosedNotReRead(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function step(){ return 1; }\nfunction update(){ return 2; }\nfunction draw(){ return 3; }\nfunction render(){ return 4; }\nfunction frame(){\n  step();\n  render();\n}\nexport function boot(){ frame(); }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/loop.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/loop.js", []map[string]any{
		{"search": "function frame(){\n  step();\n  render();\n}", "replace": "function frame(){\n  step();\n  update();\n  render();\n}"},
		{"search": "  step();\n  render();", "replace": "  step();\n  draw();"},
	})
	if res == nil || res.Success {
		t.Fatalf("the consumed anchor must still fail atomically, got %+v", res)
	}
	if res.Code != "patch_anchor_consumed" {
		t.Fatalf("Code = %q, want patch_anchor_consumed", res.Code)
	}
	data, _ := res.Data.(map[string]any)
	if data == nil || data["consumed_by_earlier_hunk"] == nil {
		t.Fatalf("reject must name the consumed anchors, got %+v", res.Data)
	}
	// nearestRegions runs against the ORIGINAL body, so for a consumed anchor it would
	// hand back the very text the caller sent, guaranteeing an identical retry.
	if data["nearest_current_regions"] != nil {
		t.Errorf("a consumed anchor must not get the re-anchor hint, got %+v", data)
	}
	for _, banned := range []string{"get the exact region", "not in the current file"} {
		if contains(res.Error, banned) {
			t.Errorf("reject must not steer to a re-read (%q), got: %s", banned, res.Error)
		}
	}

	a, _ := store.ArtifactGet(db, "file", "js/loop.js")
	if a == nil || a.Body != body {
		t.Fatalf("atomic rollback must leave the file unchanged, got %+v", a)
	}
}

// A batch with BOTH a consumed anchor and a real miss keeps the drift code (a re-anchor
// genuinely is needed) but names the consumed bucket too, so one resend fixes both.
func TestPatch_MixedConsumedAndGenuineMiss(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function step(){ return 1; }\nfunction update(){ return 2; }\nfunction draw(){ return 3; }\nfunction render(){ return 4; }\nfunction frame(){\n  step();\n  render();\n}\nexport function boot(){ frame(); }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/loop.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/loop.js", []map[string]any{
		{"search": "function frame(){\n  step();\n  render();\n}", "replace": "function frame(){\n  step();\n  update();\n  render();\n}"},
		{"search": "  step();\n  render();", "replace": "  step();\n  draw();"},
		{"search": "ZZZ_NOT_IN_THE_FILE_ANYWHERE_QQQ", "replace": "noop();"},
	})
	if res == nil || res.Success {
		t.Fatalf("want a failure, got %+v", res)
	}
	if res.Code != "patch_anchor_not_found" {
		t.Fatalf("Code = %q, want patch_anchor_not_found (a real re-anchor IS needed)", res.Code)
	}
	data, _ := res.Data.(map[string]any)
	if data == nil || data["consumed_by_earlier_hunk"] == nil {
		t.Errorf("the mixed reject must still name the consumed bucket, got %+v", res.Data)
	}
	// The re-anchor hint is for the genuine miss only.
	regions, _ := data["nearest_current_regions"].([]map[string]any)
	for _, r := range regions {
		if anchor, _ := r["anchor"].(string); contains(anchor, "step();") {
			t.Errorf("a consumed anchor must not appear in nearest_current_regions, got %+v", r)
		}
	}
}
