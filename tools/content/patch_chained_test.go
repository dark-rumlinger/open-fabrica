package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"
)

// Hunks apply in order. A later hunk whose match lies entirely inside text an
// earlier hunk of the same batch wrote would re-edit that output and silently drop
// part of it, so it is held back in Chained instead of applied.

func TestApplyPatches_SecondEditOfRewrittenRegionIsChained(t *testing.T) {
	body := "setup();\nrun();\n"
	_, res := ApplyPatches(body, []SearchReplacePatch{
		{Search: "setup();", Replace: "setup();\nextra();"},
		{Search: "setup();", Replace: "prepare();"},
	})
	if len(res.Chained) != 1 {
		t.Fatalf("a second edit to the region the first hunk rewrote must be Chained, got Chained=%v Applied=%v", res.Chained, res.Applied)
	}
}

func TestApplyPatches_EditOfFreshTextIsChained(t *testing.T) {
	body := "setup();\nrun();\n"
	_, res := ApplyPatches(body, []SearchReplacePatch{
		{Search: "setup();", Replace: "setup();\nfresh();"},
		{Search: "fresh();", Replace: "other();"},
	})
	if len(res.Chained) != 1 {
		t.Fatalf("editing text the same batch just inserted must be Chained, got Chained=%v", res.Chained)
	}
}

// Written ranges must follow later edits that shift the body.
func TestApplyPatches_ChainedSurvivesEarlierShift(t *testing.T) {
	body := "p();\nq();\nr();\n"
	_, res := ApplyPatches(body, []SearchReplacePatch{
		{Search: "r();", Replace: "r();\nz();"},
		{Search: "p();", Replace: "a_much_longer_call();"},
		{Search: "z();", Replace: "w();"},
	})
	if len(res.Chained) != 1 || len(res.Applied) != 2 {
		t.Fatalf("want the third hunk Chained after a shift, got Chained=%v Applied=%v", res.Chained, res.Applied)
	}
}

// An identical "marker" hunk followed by an insert on the same anchor is harmless:
// the marker writes nothing, so the insert applies.
func TestApplyPatches_MarkerThenInsertApplies(t *testing.T) {
	body := "setup();\nrun();\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{
		{Search: "setup();", Replace: "setup();"},
		{Search: "setup();", Replace: "setup();\nextra();"},
	})
	if len(res.Chained) != 0 || len(res.NoOp) != 1 || len(res.Applied) != 1 {
		t.Fatalf("want one no-op and one applied, got NoOp=%v Applied=%v Chained=%v", res.NoOp, res.Applied, res.Chained)
	}
	if out != "setup();\nextra();\nrun();\n" {
		t.Fatalf("got %q", out)
	}
}

// Anchors that only share a boundary line with an earlier hunk's output, and extend
// into untouched text, still apply.
func TestApplyPatches_BoundarySharingHunksApply(t *testing.T) {
	body := "one\ntwo\nthree\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{
		{Search: "one\ntwo", Replace: "ONE\ntwo"},
		{Search: "two\nthree", Replace: "two\nTHREE"},
	})
	if len(res.Chained) != 0 {
		t.Fatalf("a boundary-sharing hunk must not be Chained, got %v", res.Chained)
	}
	if out != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("got %q", out)
	}
}

// A search ending in a newline, located by a tolerant layer, takes the file's line
// break with it, as an exact match would, so no blank line is added.
func TestApplyPatches_TolerantMatchKeepsTrailingNewline(t *testing.T) {
	body := "  x = 1;  \ny = 2;\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{{Search: "  x = 1;\n", Replace: "  x = 3;\n"}})
	if len(res.Applied) != 1 {
		t.Fatalf("tolerant match should apply, got %+v", res)
	}
	if out != "  x = 3;\ny = 2;\n" {
		t.Fatalf("got %q", out)
	}
}

func TestApplyPatches_CountWarningNamesCount(t *testing.T) {
	body := "k;\nk;\nk;\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{{Search: "k;", Replace: "m;", Count: 2}})
	if out != "m;\nm;\nk;\n" {
		t.Fatalf("got %q", out)
	}
	if w := strings.Join(res.Warnings, " "); !strings.Contains(w, "replaced the first 2") {
		t.Fatalf("warning must name the count replaced, got %v", res.Warnings)
	}
}

func TestApplyPatches_NearNotFoundWarns(t *testing.T) {
	body := "k;\nk;\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{{Search: "k;", Replace: "m;", Near: "absent context"}})
	if out != "m;\nk;\n" {
		t.Fatalf("near fallback replaces the first match, got %q", out)
	}
	if w := strings.Join(res.Warnings, " "); !strings.Contains(w, "near text") {
		t.Fatalf("a missing near must warn, got %v", res.Warnings)
	}
}

func TestPatch_ChainedHunkRejectsAtomically(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := ".panel { padding: 8px; }\n.list { margin: 0; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "css/chain.css", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "file", "css/chain.css", []map[string]any{
		{"search": ".panel { padding: 8px; }", "replace": ".panel { padding: 8px; }\n.item { color: red; }"},
		{"search": ".panel { padding: 8px; }", "replace": ".item {}"},
	})
	if res == nil || res.Success || res.Code != "patch_chained_hunk" {
		t.Fatalf("want patch_chained_hunk, got %+v", res)
	}
	if contains(res.Error, "not in the current file") {
		t.Errorf("a chained reject must not claim the anchor is absent: %s", res.Error)
	}
	if a, _ := store.ArtifactGet(db, "file", "css/chain.css"); a == nil || a.Body != body {
		t.Fatalf("an atomic reject must leave the file unchanged, got %+v", a)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "css/chain.css"); v != 1 {
		t.Fatalf("an atomic reject must not bump the version, got %d", v)
	}
}

func TestPatch_NoOpRejectTeachesInsertShape(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := ".start { color: red; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "css/mark.css", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "file", "css/mark.css", []map[string]any{
		{"search": ".start { color: red; }", "replace": ".start { color: red; }"},
	})
	if res == nil || res.Code != "patch_noop_replace" {
		t.Fatalf("want patch_noop_replace, got %+v", res)
	}
	if !contains(res.Error, "no locate-only step") {
		t.Errorf("a pure no-op reject must teach the insert shape, got: %s", res.Error)
	}
}

// TestPatch_AlreadyAppliedIsIdempotentSuccess pins the build-stability fix: a hunk whose
// `search` is gone but whose `replace` end state already exists in the file was landed by a
// prior call. Re-sending it must NOT roll the batch back as an anchor miss - it is a no-op
// success so the model moves on instead of retrying a done edit.
func TestPatch_AlreadyAppliedIsIdempotentSuccess(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := ".first { margin: 1px; }\n.added-rule { padding: 2px; }\n.last { margin: 3px; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "css/done.css", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "file", "css/done.css", []map[string]any{
		{"search": ".first { margin: 1px; }\n.last { margin: 3px; }", "replace": ".first { margin: 1px; }\n.added-rule { padding: 2px; }\n.last { margin: 3px; }"},
	})
	if res == nil || !res.Success {
		t.Fatalf("want idempotent success, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if data["already_applied"] == nil {
		t.Fatalf("want already_applied surfaced, got data=%+v", data)
	}
}

// TestPatch_AlreadyAppliedDoesNotBlockSiblingEdit proves the atomicity fix: an
// already-applied hunk sent alongside a genuine edit must let that edit land, not roll the
// whole batch back.
func TestPatch_AlreadyAppliedDoesNotBlockSiblingEdit(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := ".first { margin: 1px; }\n.added-rule { padding: 2px; }\n.last { margin: 3px; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "css/mix.css", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "file", "css/mix.css", []map[string]any{
		// already applied: replace end state (3 rules) present, search (2 rules) gone
		{"search": ".first { margin: 1px; }\n.last { margin: 3px; }", "replace": ".first { margin: 1px; }\n.added-rule { padding: 2px; }\n.last { margin: 3px; }"},
		// genuine edit that must land
		{"search": ".last { margin: 3px; }", "replace": ".last { margin: 9px; }"},
	})
	if res == nil || !res.Success {
		t.Fatalf("want success with the sibling edit landed, got %+v", res)
	}
	a, _ := store.ArtifactGet(db, "file", "css/mix.css")
	if a == nil || !contains(a.Body, ".last { margin: 9px; }") {
		t.Fatalf("sibling edit did not land, got %+v", a)
	}
	data, _ := res.Data.(map[string]any)
	if data["already_applied"] == nil {
		t.Fatalf("want already_applied surfaced alongside the applied edit, got data=%+v", data)
	}
}

func TestPatch_MissDiagnosisOtherArtifact(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if res, _ := artifactActionSaveP(ctx, "file", "js/owner.js", "export const sharedMarkerValue = 42;\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	if res, _ := artifactActionSaveP(ctx, "file", "js/target.js", "const local = 1;\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "file", "js/target.js", []map[string]any{
		{"search": "export const sharedMarkerValue = 42;", "replace": "export const sharedMarkerValue = 43;"},
	})
	if res == nil || res.Code != "patch_anchor_not_found" {
		t.Fatalf("want patch_anchor_not_found, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if data["found_in_other_artifact"] == nil || !contains(res.Error, "js/owner.js") {
		t.Fatalf("want the other-artifact diagnosis naming js/owner.js, got data=%+v error=%s", data, res.Error)
	}
}

func TestPatch_MissDiagnosisNonContiguous(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := ".build {\n  margin: 0;\n  padding: 4px;\n  border: none;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "css/gap.css", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "file", "css/gap.css", []map[string]any{
		{"search": ".build {\n  margin: 0;\n  border: none;\n}", "replace": ".build {\n  border: none;\n}"},
	})
	if res == nil || res.Code != "patch_anchor_not_found" {
		t.Fatalf("want patch_anchor_not_found, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if data["non_contiguous_anchor"] == nil || !contains(res.Error, "skipped lines") {
		t.Fatalf("want the non-contiguous diagnosis, got data=%+v error=%s", data, res.Error)
	}
}

// A batch of identical hunks where one carries text the file does not hold (the
// shape of writing the desired result into both fields) is refused as a no-change
// patch, names a line of that text which IS in the file as the anchor to use, and
// never steers to a re-read.
func TestPatch_IdenticalAbsentHunkSuggestsAnchor(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := ".header { margin: 0; }\n.footer-block { padding: 4px; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "css/ident.css", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	wanted := ".new-rule { color: red; }\n\n.footer-block { padding: 4px; }"
	res, _ := artifactActionPatch(ctx, "file", "css/ident.css", []map[string]any{
		{"search": ".header { margin: 0; }", "replace": ".header { margin: 0; }"},
		{"search": wanted, "replace": wanted},
	})
	if res == nil || res.Success || res.Code != "patch_noop_replace" {
		t.Fatalf("want patch_noop_replace, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if data["unanchored_identical"] == nil || data["suggested_anchor"] == nil {
		t.Fatalf("want unanchored_identical and suggested_anchor, got %+v", data)
	}
	if !contains(res.Error, ".footer-block { padding: 4px; }") || contains(res.Error, "get the exact region") {
		t.Errorf("message must offer the kept line and skip the re-read steer: %s", res.Error)
	}
	if a, _ := store.ArtifactGet(db, "file", "css/ident.css"); a == nil || a.Body != body {
		t.Fatalf("file must be unchanged, got %+v", a)
	}
}
