package content

import (
	"fmt"
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// artifactActionGet is a test convenience over artifactActionGetImpl with the
// within-session read dedup active (fresh=false) - the shape a production get
// uses. The fresh=true escape has its own coverage.
func artifactActionGet(ctx *tools.ToolContext, kind, path string, fromLine, toLine int) (*tools.Result, error) {
	return artifactActionGetImpl(ctx, &ArtGet{Kind: kind, FilePath: path, FromLine: fromLine, ToLine: toLine})
}

// setupFullArtifactsDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupFullArtifactsDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

// insertArtifactRow writes a row directly, bypassing ArtifactSave's kind-specific
// path validation so tests can stage a cross-kind path collision (and control
// size/updated_at, which drive the read-cache fingerprint).
func insertArtifactRow(t *testing.T, db dbh.Handle, kind, path, body, updatedAt string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO of_files (kind, path, body, scope, content_type, storage, disk_path, size, metadata, status, is_deleted, created_at, updated_at)
		 VALUES (?, ?, ?, '', '', 'inline', '', ?, '{}', 'completed', 0, ?, ?)`,
		kind, path, body, len(body), updatedAt, updatedAt,
	)
	if err != nil {
		t.Fatalf("insert %s/%s: %v", kind, path, err)
	}
}

// TestArtifactDeleteGuardsPlatformManaged pins that a file carrying the
// platform_managed metadata flag cannot be deleted by the model, while
// ordinary artifacts - including the model-authored js/auth.js and js/e2ee.js
// of the engine era - delete normally (the guides warn the model off; the
// runtime probe catches an actual breakage).
func TestArtifactDeleteGuardsPlatformManaged(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "js/e2ee.js", "export const session=1;", "2026-06-01T14:00:00Z")
	insertArtifactRow(t, db, "file", "js/auth.js", "export const requireRole=1;", "2026-06-01T14:00:00Z")
	insertArtifactRow(t, db, "file", "js/call.js", "console.log(1)", "2026-06-01T14:00:00Z")
	// A platform artifact: protected purely by the metadata flag.
	if _, err := db.Exec(
		`INSERT INTO of_files (kind, path, body, scope, content_type, storage, disk_path, size, metadata, status, is_deleted, created_at, updated_at)
		 VALUES ('file','js/future.js','x','global','','inline','',1,'{"platform_managed":true}','completed',0,'t','t')`,
	); err != nil {
		t.Fatalf("seed platform-managed file: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, _ := artifactActionDelete(ctx, "file", "js/future.js")
	if res.Success {
		t.Fatal("deleting a platform-managed (flagged) file must be refused")
	}
	var d int
	db.QueryRow("SELECT is_deleted FROM of_files WHERE kind='file' AND path='js/future.js'").Scan(&d)
	if d != 0 {
		t.Fatalf("js/future.js must remain (is_deleted=%d)", d)
	}

	// Model-authored files delete like any other artifact.
	for _, p := range []string{"js/e2ee.js", "js/auth.js", "js/call.js"} {
		res, _ := artifactActionDelete(ctx, "file", p)
		if !res.Success {
			t.Fatalf("deleting ordinary artifact %s should succeed, got %q", p, res.Error)
		}
	}
}

// Deleting a file takes it off every page it was attached to. Left attached, a deleted
// temporary probe kept /staff and /login loading a URL that 404s, and a whole repair
// round went to finding that load failure.
func TestArtifactDeleteDetachesFileFromPages(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "_probe.js", "console.log(1)", "2026-06-01T14:00:00Z")
	insertArtifactRow(t, db, "file", "js/staff.js", "console.log(2)", "2026-06-01T14:00:00Z")
	for _, l := range [][2]string{{"/staff", "_probe.js"}, {"/login", "_probe.js"}, {"/staff", "js/staff.js"}} {
		if err := store.LinkPageAsset(db, l[0], l[1], 0); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := artifactActionDelete(&tools.ToolContext{DB: db}, "file", "_probe.js")
	if !res.Success {
		t.Fatalf("delete failed: %s", res.Error)
	}
	if data, _ := res.Data.(map[string]any); fmt.Sprint(data["detached_from"]) != "[/login /staff]" {
		t.Errorf("detached_from = %v, want both pages", data["detached_from"])
	}
	if pages, _ := store.PagesAttaching(db, "_probe.js"); len(pages) != 0 {
		t.Errorf("deleted file is still attached to %v", pages)
	}
	if assets, _ := store.PageAssets(db, "/staff"); len(assets) != 1 || assets[0] != "js/staff.js" {
		t.Errorf("/staff assets = %v, want only js/staff.js", assets)
	}
}

// TestArtifactGetInfersKind covers Fix 1: a read with no kind resolves the kind
// from the path when unambiguous, and refuses (without guessing) when the path
// maps to more than one kind.
func TestArtifactGetInfersKind(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "page", "/", "<h1>home</h1>", "2026-06-01T14:00:00Z")
	insertArtifactRow(t, db, "file", "js/main.js", "console.log(1)", "2026-06-01T14:00:00Z")
	insertArtifactRow(t, db, "page", "dup", "p", "2026-06-01T14:00:00Z")
	insertArtifactRow(t, db, "component", "dup", "c", "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db}

	// Unambiguous path, no kind supplied: resolves and returns the body.
	res, _ := artifactActionGet(ctx, "", "js/main.js", 0, 0)
	if !res.Success {
		t.Fatalf("expected success inferring kind for js/main.js, got %q", res.Error)
	}
	if data, ok := res.Data.(map[string]any); !ok || data["kind"] != "file" {
		t.Fatalf("expected inferred kind=file, got %+v", res.Data)
	}

	// Ambiguous path: must refuse and name the candidates rather than guess.
	res, _ = artifactActionGet(ctx, "", "dup", 0, 0)
	if res.Success || res.Code != "ambiguous_kind" {
		t.Fatalf("expected ambiguous_kind failure for dup, got success=%v code=%q", res.Success, res.Code)
	}

	// Explicit kind still works and disambiguates.
	res, _ = artifactActionGet(ctx, "component", "dup", 0, 0)
	if !res.Success {
		t.Fatalf("explicit kind should resolve dup, got %q", res.Error)
	}
}

// TestArtifactGetOutlineFirstForLargeFile pins the outline-first default: a bare
// get (no range) of a file too large to return in one page comes back as the
// structural outline, not the body, and is NOT recorded as delivered - so an
// explicit range afterwards still serves the real source.
func TestArtifactGetOutlineFirstForLargeFile(t *testing.T) {
	db := setupFullArtifactsDB(t)
	// Body well over getBodyBudget whose outline collapses to a handful of lines.
	big := "export function setup(){\n" + strings.Repeat("  doThing();\n", 3000) + "}\n"
	insertArtifactRow(t, db, "file", "js/big.js", big, "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	// Bare get: returns the outline map, not the body.
	res, _ := artifactActionGet(ctx, "file", "js/big.js", 0, 0)
	data := res.Data.(map[string]any)
	if data["outline"] == nil || data["body"] != nil {
		t.Fatalf("bare get of a large file should return an outline, not a body, got %+v", data)
	}

	// Not recorded as delivered: an explicit range serves the real source (not a stub).
	res, _ = artifactActionGet(ctx, "file", "js/big.js", 1, 50)
	data = res.Data.(map[string]any)
	if data["body"] == nil || data["unchanged"] != nil {
		t.Fatalf("an explicit range after an outline-first get must serve real source, got %+v", data)
	}

	// A small file is unaffected: a bare get still serves its full body.
	insertArtifactRow(t, db, "file", "js/small.js", "export const x = 1;\n", "2026-06-01T14:00:00Z")
	res, _ = artifactActionGet(ctx, "file", "js/small.js", 0, 0)
	data = res.Data.(map[string]any)
	if data["body"] == nil || data["outline"] != nil {
		t.Fatalf("bare get of a small file should serve its full body, got %+v", data)
	}
}

// TestArtifactGetOutlineTriggerBelowPageBudget pins the outline trigger for
// mid-size bodies: a bare get of a body between outlineTrigger and getBodyBudget
// (e.g. ~20KB of dense JS) returns the outline rather than the full body. Serving
// the full body here would let the line-number gutter push it over the page
// budget, truncating it mid-file with no outline and no tail.
func TestArtifactGetOutlineTriggerBelowPageBudget(t *testing.T) {
	db := setupFullArtifactsDB(t)
	// ~20KB raw: over outlineTrigger, under getBodyBudget.
	mid := "export function setup(){\n" + strings.Repeat("  doThing();\n", 1600) + "}\n"
	if len(mid) <= outlineTrigger || len(mid) > getBodyBudget {
		t.Fatalf("test body must sit between outlineTrigger (%d) and getBodyBudget (%d), got %d", outlineTrigger, getBodyBudget, len(mid))
	}
	insertArtifactRow(t, db, "file", "js/mid.js", mid, "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, _ := artifactActionGet(ctx, "file", "js/mid.js", 0, 0)
	data := res.Data.(map[string]any)
	if data["outline"] == nil || data["body"] != nil {
		t.Fatalf("bare get of a body over outlineTrigger should return an outline, got %+v", data)
	}

	// An explicit range still serves the real source.
	res, _ = artifactActionGet(ctx, "file", "js/mid.js", 1, 50)
	data = res.Data.(map[string]any)
	if data["body"] == nil {
		t.Fatalf("explicit range must serve real source, got %+v", data)
	}
}

// TestArtifactGetReadCache covers the read-dedup: once the whole body has been
// delivered this session, a repeat read - full OR ranged - of the unchanged
// file returns a stub, while a post-edit read serves the body in full again.
func TestArtifactGetReadCache(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "js/main.js", "a\nb\nc\nd", "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	// First full read: serves the body.
	res, _ := artifactActionGet(ctx, "file", "js/main.js", 0, 0)
	if data := res.Data.(map[string]any); data["body"] == nil || data["unchanged"] != nil {
		t.Fatalf("first read should serve body, got %+v", data)
	}

	// Every repeat of the unchanged file is stubbed for as long as the body is
	// resident (ranged reads too - the model already holds the whole body). The
	// dedup is eviction-aware, not self-healing-by-flip-flop: it must not re-serve
	// a body that is still in context, which would restart the re-read spiral.
	for i, win := range [][2]int{{2, 3}, {0, 0}, {0, 0}} {
		res, _ = artifactActionGet(ctx, "file", "js/main.js", win[0], win[1])
		if data := res.Data.(map[string]any); data["unchanged"] != true || data["body"] != nil {
			t.Fatalf("repeat %d of a resident unchanged file should stub, got %+v", i, data)
		}
	}

	// After an edit the fingerprint (updated_at|size) changes, so the next read
	// serves again rather than stubbing - the model genuinely needs the new body.
	if _, err := db.Exec(`UPDATE of_files SET body = ?, size = ?, updated_at = ? WHERE kind='file' AND path='js/main.js'`, "a\nb\nc\nd\nE", 9, "2026-06-01T15:00:00Z"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	res, _ = artifactActionGet(ctx, "file", "js/main.js", 0, 0)
	if data := res.Data.(map[string]any); data["body"] == nil || data["unchanged"] != nil {
		t.Fatalf("post-edit read should serve body, got %+v", data)
	}
}

// TestArtifactGetRangedNeverFullServed pins the safety invariant: a file the
// model has only ever seen in slices (never delivered whole) is never stubbed,
// so a ranged read it actually needs always serves. Only a complete, untruncated
// delivery arms the dedup.
func TestArtifactGetRangedNeverFullServed(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "js/main.js", "a\nb\nc\nd\ne\nf", "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	// Two ranged reads, never a full one: both serve (nothing was ever recorded).
	for _, win := range [][2]int{{1, 2}, {3, 4}} {
		res, _ := artifactActionGet(ctx, "file", "js/main.js", win[0], win[1])
		if data := res.Data.(map[string]any); data["body"] == nil || data["unchanged"] != nil {
			t.Fatalf("ranged read of a never-fully-delivered file should serve (window %v), got %+v", win, data)
		}
	}
}

// TestArtifactGetPreMarkedDelivered covers the build-loop pre-seed: when the
// loop has recorded a body as delivered (because it inlined it into a pass
// seed), the model's first get of that unchanged file - full or ranged - is
// stubbed, even though this session never read it via get. This is what stops a
// capability pass re-reading the entry module it was handed in its seed.
func TestArtifactGetPreMarkedDelivered(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "js/main.js", "a\nb\nc\nd", "2026-06-01T14:00:00Z")
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}

	// Simulate the build loop pre-marking the inlined body, using the same key +
	// fingerprint the read handler computes.
	a, err := store.ArtifactGet(db, "file", "js/main.js")
	if err != nil || a == nil {
		t.Fatalf("seed fetch: %v", err)
	}
	rt.RecordDelivered(tools.ReadCacheKey("file", "js/main.js"), tools.ReadFingerprint(a.Body))

	// First-ever get this session is stubbed (body is in the seed, not re-sent).
	res, _ := artifactActionGet(ctx, "file", "js/main.js", 0, 0)
	if data := res.Data.(map[string]any); data["unchanged"] != true || data["body"] != nil {
		t.Fatalf("pre-marked file should stub on first get, got %+v", data)
	}
}

// TestCanonicalizeFilePath pins the manage_files path-canonicalisation rule: a
// /files/ URL form is collapsed to the bare stored path and forces kind=file
// (the prefix is an unambiguous file signal), an explicit kind=file trims a
// stray leading slash, and a page path is left exactly as given so document
// kinds keep their "/" paths.
func TestCanonicalizeFilePath(t *testing.T) {
	cases := []struct {
		inKind, inPath     string
		wantKind, wantPath string
	}{
		{"", "/files/viewport.js", "file", "viewport.js"},
		{"", "files/viewport.js", "file", "viewport.js"},
		{"file", "/viewport.js", "file", "viewport.js"},
		{"file", "viewport.js", "file", "viewport.js"},
		{"page", "/about", "page", "/about"},
		{"", "/about", "", "/about"},
		// An explicit non-file kind is never overridden by a /files/-shaped path.
		{"page", "/files/weird", "page", "/files/weird"},
	}
	for _, c := range cases {
		gotKind, gotPath := canonicalizeFilePath(c.inKind, c.inPath)
		if gotKind != c.wantKind || gotPath != c.wantPath {
			t.Errorf("canonicalizeFilePath(%q,%q) = (%q,%q), want (%q,%q)",
				c.inKind, c.inPath, gotKind, gotPath, c.wantKind, c.wantPath)
		}
	}
}

// TestArtifactGetURLFormHitsDedup is the read-dedup regression: a body delivered
// under its bare path must still stub when the model re-reads it via the /files/
// URL form. Before path canonicalisation the two forms keyed different cache
// entries, so every /files/ re-read re-sent the whole body - the dominant
// re-read waste this fix targets.
func TestArtifactGetURLFormHitsDedup(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "viewport.js", "a\nb\nc\nd", "2026-06-01T14:00:00Z")
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}

	a, err := store.ArtifactGet(db, "file", "viewport.js")
	if err != nil || a == nil {
		t.Fatalf("seed fetch: %v", err)
	}
	rt.RecordDelivered(tools.ReadCacheKey("file", "viewport.js"), tools.ReadFingerprint(a.Body))

	// Re-read via the URL form the model often emits: it must resolve to the same
	// row and be stubbed, not re-served in full.
	res, _ := artifactActionGet(ctx, "", "/files/viewport.js", 0, 0)
	data, ok := res.Data.(map[string]any)
	if !ok || data["unchanged"] != true || data["body"] != nil {
		t.Fatalf("/files/ re-read of a delivered body should stub, got %+v", res.Data)
	}
}

// A save registers the written body as delivered: the model authored every
// byte, so a reflexive get right after the save stubs instead of re-sending
// them. And because the fingerprint hashes content, a metadata-only row touch
// (what a page-asset link or scope promotion does) must not break the dedup.
func TestSaveRegistersReadDedupSurvivesMetadataTouch(t *testing.T) {
	db := setupFullArtifactsDB(t)
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}

	res, _ := artifactActionSaveP(ctx, "file", "js/board.js", "export const board = [];\n", "", "global", nil, "", false)
	if !res.Success {
		t.Fatalf("save failed: %q", res.Error)
	}

	get, _ := artifactActionGet(ctx, "file", "js/board.js", 0, 0)
	if data := get.Data.(map[string]any); data["unchanged"] != true || data["body"] != nil {
		t.Fatalf("get after save should stub (model holds the bytes it wrote), got %+v", data)
	}

	// Metadata-only touch: updated_at shifts, body does not. Dedup must hold.
	if _, err := db.Exec(`UPDATE of_files SET updated_at = '2030-01-01T00:00:00Z' WHERE path = 'js/board.js'`); err != nil {
		t.Fatalf("touch: %v", err)
	}
	get2, _ := artifactActionGet(ctx, "file", "js/board.js", 0, 0)
	if data := get2.Data.(map[string]any); data["unchanged"] != true || data["body"] != nil {
		t.Fatalf("get after a metadata-only touch should still stub, got %+v", data)
	}
}

// An in-place edit of a small file keeps it resident: the edit echo (expanded by
// context) spans the whole tiny body, so the echoed line range covers a
// subsequent full get and it stubs instead of re-serving a file the model just
// edited. Residence is the lines the model held before the edit plus the echoed
// spans - see TestChainEditsCarryReadResidence for how far that carries.
func TestEditTransfersReadResidence(t *testing.T) {
	db := setupFullArtifactsDB(t)
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}

	if res, _ := artifactActionSaveP(ctx, "file", "js/score.js", "let score = 0;\nlet lines = 0;\n", "", "global", nil, "", false); !res.Success {
		t.Fatalf("save failed: %q", res.Error)
	}
	patch, _ := artifactActionPatch(ctx, "file", "js/score.js", []map[string]any{
		{"search": "let lines = 0;", "replace": "let lines = 0;\nlet level = 1;"},
	})
	if !patch.Success {
		t.Fatalf("patch failed: %q", patch.Error)
	}

	get, _ := artifactActionGet(ctx, "file", "js/score.js", 0, 0)
	if data := get.Data.(map[string]any); data["unchanged"] != true || data["body"] != nil {
		t.Fatalf("get after an edit of a fully-held body should stub, got %+v", data)
	}
}

// A body the model only ever saw in part does not transfer across an edit: the
// post-edit get must re-serve in full so the model regains the whole file.
func TestPartialReadDoesNotTransferAcrossEdit(t *testing.T) {
	db := setupFullArtifactsDB(t)
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}

	insertArtifactRow(t, db, "file", "js/long.js", "l1\nl2\nl3\nl4\nl5\nl6", "2026-06-01T14:00:00Z")
	if res, _ := artifactActionGet(ctx, "file", "js/long.js", 1, 2); !res.Success {
		t.Fatalf("ranged get failed: %q", res.Error)
	}
	patch, _ := artifactActionPatch(ctx, "file", "js/long.js", []map[string]any{
		{"search": "l6", "replace": "l6\nl7"},
	})
	if !patch.Success {
		t.Fatalf("patch failed: %q", patch.Error)
	}

	get, _ := artifactActionGet(ctx, "file", "js/long.js", 0, 0)
	data := get.Data.(map[string]any)
	if data["unchanged"] == true || data["body"] == nil {
		t.Fatalf("get after an edit of a partially-read body must serve in full, got %+v", data)
	}
}

// After a full read and a CHAIN of edits, the model still holds every line it read
// that the edits did not touch (the segment transcript is append-only), so a get of a
// range far from every edit stubs instead of re-sending bytes it already has. A range
// it NEVER read must still serve in full: residence is carried over, never invented.
func TestChainEditsCarryReadResidence(t *testing.T) {
	db := setupFullArtifactsDB(t)
	var sb strings.Builder
	for i := 1; i <= 60; i++ {
		sb.WriteString("const v" + itoa(i) + " = " + itoa(i) + ";\n")
	}
	body := sb.String()
	edits := []map[string]any{
		{"search": "const v2 = 2;", "replace": "const v2 = 200;"},
		{"search": "const v3 = 3;", "replace": "const v3 = 300;"},
		{"search": "const v4 = 4;", "replace": "const v4 = 400;"},
	}
	chain := func(ctx *tools.ToolContext) {
		t.Helper()
		for _, p := range edits {
			if res, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{p}); !res.Success {
				t.Fatalf("patch failed: %q", res.Error)
			}
		}
	}

	// Full read, then the chain: a far range stubs, and so does the echoed window.
	insertArtifactRow(t, db, "file", "js/mod.js", body, "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if res, _ := artifactActionGet(ctx, "file", "js/mod.js", 0, 0); !res.Success {
		t.Fatalf("full read failed: %q", res.Error)
	}
	chain(ctx)
	for _, r := range [][2]int{{50, 60}, {3, 4}} {
		res, _ := artifactActionGet(ctx, "file", "js/mod.js", r[0], r[1])
		if d := res.Data.(map[string]any); d["unchanged"] != true || d["body"] != nil {
			t.Fatalf("lines %d-%d were read before the edits and are unchanged: want a stub, got %+v", r[0], r[1], d)
		}
	}

	// Only the top was ever read: the far range was never held, so it serves in full.
	db2 := setupFullArtifactsDB(t)
	insertArtifactRow(t, db2, "file", "js/mod.js", body, "2026-06-01T14:00:00Z")
	ctx2 := &tools.ToolContext{DB: db2, ReadTracker: tools.NewReadTracker()}
	if res, _ := artifactActionGet(ctx2, "file", "js/mod.js", 1, 10); !res.Success {
		t.Fatalf("ranged read failed: %q", res.Error)
	}
	chain(ctx2)
	res, _ := artifactActionGet(ctx2, "file", "js/mod.js", 50, 60)
	if d := res.Data.(map[string]any); d["unchanged"] == true || d["body"] == nil {
		t.Fatalf("a range never read must serve in full, got %+v", d)
	}
}

// TestPatchAnchorMiss_DriftCauseByKind pins the kind-aware cause in the
// patch_anchor_not_found message. A page/layout/component is stored verbatim, so its miss
// must NOT blame import auto-wire (an impossible cause that sends the model down a wrong
// recovery); a JS module CAN be auto-wired between read and patch, so its message keeps
// naming it. Either way the re-get and re-save recovery steers stay intact.
func TestPatchAnchorMiss_DriftCauseByKind(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	// A page: verbatim storage, so the miss cause must not mention auto-wire.
	if res, _ := artifactActionSaveP(ctx, "page", "/", "<main>\n  <h1>Home</h1>\n</main>\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("seed page save failed: %q", res.Error)
	}
	res, _ := artifactActionPatch(ctx, "page", "/", []map[string]any{
		{"search": "ZZZ_NOT_IN_THE_PAGE_ANYWHERE_QQQ", "replace": "x"},
	})
	if res.Success || res.Code != "patch_anchor_not_found" {
		t.Fatalf("a no-match page patch must fail with patch_anchor_not_found, got success=%v code=%q", res.Success, res.Code)
	}
	if strings.Contains(res.Error, "auto-wired") {
		t.Errorf("a page miss must NOT blame auto-wire (pages are stored verbatim), got: %s", res.Error)
	}
	if !strings.Contains(res.Error, "out-of-date copy") {
		t.Errorf("a page miss must name the stale-draft cause, got: %s", res.Error)
	}
	if !strings.Contains(res.Error, "copy the anchor verbatim") || !strings.Contains(res.Error, "re-save the whole corrected body") {
		t.Errorf("a miss message must keep both recovery steers regardless of kind, got: %s", res.Error)
	}

	// A JS module: can be auto-wired between read and patch, so keep naming it.
	if res, _ := artifactActionSaveP(ctx, "file", "js/app.js", "export const a = 1;\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("seed js save failed: %q", res.Error)
	}
	res, _ = artifactActionPatch(ctx, "file", "js/app.js", []map[string]any{
		{"search": "ZZZ_NOT_IN_THE_MODULE_ANYWHERE_QQQ", "replace": "x"},
	})
	if res.Success || res.Code != "patch_anchor_not_found" {
		t.Fatalf("a no-match js patch must fail with patch_anchor_not_found, got success=%v code=%q", res.Success, res.Code)
	}
	if !strings.Contains(res.Error, "auto-wired") {
		t.Errorf("a JS-module miss must still name auto-wire as a possible cause, got: %s", res.Error)
	}
}

// TestPatchAnchorMiss_NamesPreviousVersionMatch: an anchor that matches the
// file's PREVIOUS stored version is the model quoting bytes its own earlier
// edit replaced - the most common real anchor-miss cause. The miss message
// must say so instead of leaving the model to guess between drift causes.
func TestPatchAnchorMiss_NamesPreviousVersionMatch(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	header := "export const zoom = 1;\n"
	legacy := "export function legacyRenderPath(ctx2d) {\n  const buf = [];\n  buf.push(ctx2d);\n  return \"legacy\";\n}\n"
	footer := "export function draw() {}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/bricks.js", header+legacy+footer, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed v1 failed: %q", res.Error)
	}
	// v2 removes the legacy block entirely.
	if res, _ := artifactActionSaveP(ctx, "file", "js/bricks.js", header+footer, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed v2 failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/bricks.js", []map[string]any{
		{"search": legacy, "replace": "export function renderPath() {}\n"},
	})
	if res.Success || res.Code != "patch_anchor_not_found" {
		t.Fatalf("stale anchor must miss with patch_anchor_not_found, got success=%v code=%q err=%q", res.Success, res.Code, res.Error)
	}
	if !strings.Contains(res.Error, "PREVIOUS version") {
		t.Errorf("miss message should name the previous-version match, got: %s", res.Error)
	}
	data, _ := res.Data.(map[string]any)
	if n, _ := data["anchors_match_previous_version"].(int); n != 1 {
		t.Errorf("anchors_match_previous_version = %v, want 1", data["anchors_match_previous_version"])
	}

	// A genuinely absent anchor (never in any version) must NOT claim it.
	res, _ = artifactActionPatch(ctx, "file", "js/bricks.js", []map[string]any{
		{"search": "ZZZ_NEVER_IN_ANY_VERSION_QQQ", "replace": "x"},
	})
	if res.Success || strings.Contains(res.Error, "PREVIOUS version") {
		t.Errorf("a never-existed anchor must not be blamed on the previous version, got: %s", res.Error)
	}
}

// TestPatchAtomicOnUnmatchedAnchor pins the atomic-patch contract: a multi-hunk
// patch where ANY anchor is not found applies NOTHING and fails, instead of
// silently committing the matched hunks under success:true. The half-applied
// shape (a call site rewritten while its matching new body never lands) would
// otherwise leave a dead remnant and force a get -> patch -> get thrash to find
// and clean it up.
func TestPatchAtomicOnUnmatchedAnchor(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "function send() {\n  doSend();\n}\nfunction doSend() {}\nfunction broadcast() {}\n\nconst x = 1;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/chat.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	// One real anchor + one that does not exist (and is too distinct to fuzzy-match).
	res, _ := artifactActionPatch(ctx, "file", "js/chat.js", []map[string]any{
		{"search": "doSend();", "replace": "broadcast();"},
		{"search": "ZZZ_NOT_IN_THE_FILE_ANYWHERE_QQQ", "replace": "noop();"},
	})
	if res.Success || res.Code != "patch_anchor_not_found" {
		t.Fatalf("a patch with an unmatched anchor must fail with code patch_anchor_not_found, got success=%v code=%q", res.Success, res.Code)
	}
	if data, ok := res.Data.(map[string]any); !ok || data["not_found"] == nil {
		t.Fatalf("failed patch must report not_found anchors, got %+v", res.Data)
	}
	// The "N of M" count is the hunks the caller sent, not a total reconstructed from
	// the result buckets - a hunk that lands in no bucket would silently shrink M.
	if !strings.Contains(res.Error, "1 of 2") {
		t.Errorf("miss count should be 1 of 2 (the hunks sent), got: %s", res.Error)
	}
	// The matched hunks are reported under `matched_rolled_back`, NEVER `applied`: an
	// atomic failure persists nothing, and a field named `applied` on a failure makes the
	// model believe those hunks landed and patch against a phantom file it never wrote.
	if data, _ := res.Data.(map[string]any); data["applied"] != nil {
		t.Errorf("a failed atomic patch must not report an `applied` field (reads as landed); use matched_rolled_back, got %+v", data)
	} else if data["matched_rolled_back"] == nil {
		t.Errorf("a failed atomic patch must report the matched-but-rolled-back hunks under matched_rolled_back, got %+v", data)
	}

	// The matched hunk must NOT have been committed: the file is byte-for-byte the seed.
	a, err := store.ArtifactGet(db, "file", "js/chat.js")
	if err != nil || a == nil {
		t.Fatalf("fetch after failed patch: %v", err)
	}
	if a.Body != body {
		t.Fatalf("atomic patch must leave the file unchanged on any miss, got %q", a.Body)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "js/chat.js"); v != 1 {
		t.Fatalf("a failed atomic patch must not bump the version, got %d", v)
	}

	// The symmetric case: every anchor matches -> applies and bumps the version.
	res2, _ := artifactActionPatch(ctx, "file", "js/chat.js", []map[string]any{
		{"search": "doSend();", "replace": "broadcast();"},
		{"search": "const x = 1;", "replace": "const x = 2;"},
	})
	if !res2.Success {
		t.Fatalf("a fully-matched patch should apply, got %q", res2.Error)
	}
	a2, _ := store.ArtifactGet(db, "file", "js/chat.js")
	if !strings.Contains(a2.Body, "broadcast();") || !strings.Contains(a2.Body, "const x = 2;") {
		t.Fatalf("fully-matched patch must apply every hunk, got %q", a2.Body)
	}
}

// TestPatchNoOpReplaceIsDistinctFromMiss pins the no-op taxonomy: a patch whose
// `replace` equals its `search` (the anchor IS present) is NOT an anchor miss. It
// must fail under its own code, steer the model to fix `replace` (not re-anchor or
// re-save), and never masquerade as patch_anchor_not_found - the mislabel that
// drives patch thrash: repeated "anchor not found / re-read / re-save" calls for
// what is really a no-op replace.
func TestPatchNoOpReplaceIsDistinctFromMiss(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "import { a } from 'x';\nexport function f() { return a; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/mod.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	// A pure no-op: the anchor exists, but replace == search, so nothing changes.
	res, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{
		{"search": "import { a } from 'x';", "replace": "import { a } from 'x';"},
	})
	if res.Success || res.Code != "patch_noop_replace" {
		t.Fatalf("a no-op replace (anchor present) must fail with code patch_noop_replace, got success=%v code=%q", res.Success, res.Code)
	}
	// The steer must be the OPPOSITE of the miss steer: keep the anchor, fix replace.
	// It must not repeat the miss message's false claims (anchor "not found", text
	// "not in the current file") or its re-send-whole-file remedy.
	for _, bad := range []string{"not found", "not in the current file", "re-send the WHOLE"} {
		if strings.Contains(res.Error, bad) {
			t.Errorf("no-op message must not carry the miss steer %q; got: %s", bad, res.Error)
		}
	}
	if !strings.Contains(res.Error, "do NOT re-read") || !strings.Contains(res.Error, "ARE in the file") {
		t.Errorf("no-op message must affirm the anchor is correct and steer away from re-reading; got: %s", res.Error)
	}
	// It must NOT count against the anchor-miss guardrail: the session counter fires
	// only on code == "patch_anchor_not_found", and this code is deliberately distinct.
	// File unchanged, version not bumped.
	if a, _ := store.ArtifactGet(db, "file", "js/mod.js"); a.Body != body {
		t.Fatalf("a no-op patch must leave the file byte-identical, got %q", a.Body)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "js/mod.js"); v != 1 {
		t.Fatalf("a no-op patch must not bump the version, got %d", v)
	}

	// A search==replace whose text is absent changes nothing wherever it would land,
	// so it is a no-change reject too, never the re-read steer of an anchor miss.
	resMiss, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{
		{"search": "ZZZ_ABSENT_QQQ", "replace": "ZZZ_ABSENT_QQQ"},
	})
	if resMiss.Success || resMiss.Code != "patch_noop_replace" {
		t.Fatalf("identical search/replace whose text is absent must be patch_noop_replace, got success=%v code=%q", resMiss.Success, resMiss.Code)
	}
	if strings.Contains(resMiss.Error, "get the exact region") {
		t.Errorf("an identical hunk must not get the re-read steer: %s", resMiss.Error)
	}

	// Mixed batch: a real miss dominates the code, but the no-op is still named so
	// the model fixes both in one resend.
	resMix, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{
		{"search": "import { a } from 'x';", "replace": "import { a } from 'x';"}, // no-op
		{"search": "ZZZ_ABSENT_QQQ", "replace": "ok();"},                          // real miss
	})
	if resMix.Success || resMix.Code != "patch_anchor_not_found" {
		t.Fatalf("a batch with a real miss must fail with patch_anchor_not_found, got code=%q", resMix.Code)
	}
	if d, _ := resMix.Data.(map[string]any); d["no_op"] == nil {
		t.Errorf("a mixed batch must still surface the no-op edits under no_op, got %+v", resMix.Data)
	}
}

// After a second consecutive no-change patch on one file, the reject escalates
// under its own streak (noop_loop), separate from the anchor-miss guardrail,
// and a successful write clears it.
func TestPatchNoOpLoopEscalates(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := "export const x = 1;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/mod.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	noop := []map[string]any{{"search": "export const x = 1;", "replace": "export const x = 1;"}}

	// First no-op: the base message, no escalation.
	r1, _ := artifactActionPatch(ctx, "file", "js/mod.js", noop)
	if r1.Success || r1.Code != "patch_noop_replace" {
		t.Fatalf("first no-op should be patch_noop_replace, got success=%v code=%q", r1.Success, r1.Code)
	}
	if d, _ := r1.Data.(map[string]any); d["noop_loop"] != nil {
		t.Errorf("first no-op must not escalate, got noop_loop=%v", d["noop_loop"])
	}
	if strings.Contains(r1.Error, "STOP:") {
		t.Errorf("first no-op must not carry the STOP escalation: %s", r1.Error)
	}

	// Second consecutive no-op on the same file escalates under its own key.
	r2, _ := artifactActionPatch(ctx, "file", "js/mod.js", noop)
	d2, _ := r2.Data.(map[string]any)
	if d2 == nil || d2["noop_loop"] == nil {
		t.Fatalf("second consecutive no-op must escalate (noop_loop set), got %+v", r2.Data)
	}
	if !strings.Contains(r2.Error, "STOP:") {
		t.Errorf("escalation must carry a decisive stop: %s", r2.Error)
	}
	if d2["edit_loop"] != nil {
		t.Errorf("a no-op streak must not set the anchor-miss escalation, got edit_loop=%v", d2["edit_loop"])
	}

	// A successful patch clears the streak, so a later isolated no-op starts fresh.
	if res, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{
		{"search": "export const x = 1;", "replace": "export const x = 2;"},
	}); !res.Success {
		t.Fatalf("real patch failed: %q", res.Error)
	}
	r3, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{
		{"search": "export const x = 2;", "replace": "export const x = 2;"},
	})
	if d, _ := r3.Data.(map[string]any); d["noop_loop"] != nil {
		t.Errorf("a successful write must clear the no-op streak, got noop_loop=%v", d["noop_loop"])
	}
}

// The no-op loop also runs distributed: a file hit with no-op patches many times
// across a build, each reset by a real edit in between, never twice in a row. The
// consecutive streak never fires, so the cumulative count must escalate once it
// crosses noopCumulativeStop. This is the shape that burned a real build.
func TestPatchNoOpLoopEscalatesDistributed(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if res, _ := artifactActionSaveP(ctx, "file", "js/mod.js", "export const x = 0;\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	// Each round: one no-op (consecutive stays 1, cleared by the real edit that follows),
	// then a real edit that bumps the value. The cumulative no-op count climbs anyway.
	var last *tools.Result
	for i := 0; i < noopCumulativeStop; i++ {
		cur := fmt.Sprintf("export const x = %d;", i)
		last, _ = artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{{"search": cur, "replace": cur}})
		if last.Code != "patch_noop_replace" {
			t.Fatalf("round %d: want patch_noop_replace, got %q", i, last.Code)
		}
		// Advance the body so the next no-op anchors on fresh text and the streak clears.
		next := fmt.Sprintf("export const x = %d;", i+1)
		if res, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{{"search": cur, "replace": next}}); !res.Success {
			t.Fatalf("round %d real edit failed: %q", i, res.Error)
		}
	}
	d, _ := last.Data.(map[string]any)
	if d == nil || d["noop_total"] == nil {
		t.Fatalf("the %dth distributed no-op must escalate on the cumulative count (noop_total set), got %+v", noopCumulativeStop, last.Data)
	}
	if !strings.Contains(last.Error, "STOP:") || !strings.Contains(last.Error, "across this build") {
		t.Errorf("a distributed escalation must name the across-build pattern: %s", last.Error)
	}
}

// A batch mixing a no-op hunk with hunks that DO change bytes must land the real
// hunks and surface the no-ops on the success result, never reject the batch.
// Atomic rejection of mixed batches is what cascaded: the model retried assuming
// the real hunks had landed and hit undefined_reference on the missing half.
func TestPatchMixedNoOpBatchLands(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := "export const x = 1;\nexport function f() { return x; }\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/mod.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/mod.js", []map[string]any{
		{"search": "export const x = 1;", "replace": "export const x = 1;"}, // no-op
		{"search": "return x;", "replace": "return x + 1;"},                 // real
	})
	if !res.Success {
		t.Fatalf("a mixed no-op batch must land its real hunks, got code=%q error=%q", res.Code, res.Error)
	}
	a, _ := store.ArtifactGet(db, "file", "js/mod.js")
	if !strings.Contains(a.Body, "return x + 1;") {
		t.Fatalf("real hunk must have applied, got %q", a.Body)
	}
	d, _ := res.Data.(map[string]any)
	if d["no_op"] == nil || d["no_op_note"] == nil {
		t.Errorf("success result must surface the no-op hunks (no_op + no_op_note), got %+v", res.Data)
	}
	if note, _ := d["no_op_note"].(string); !strings.Contains(note, "landed") {
		t.Errorf("no_op_note must say the other edits landed, got %q", note)
	}
}

// A hunk whose replace differs from its search only by whitespace the tolerant
// matcher normalizes away "applies" but leaves the body byte-identical. That must
// reject as a no-op, not succeed with a version bump and no `updated` echo - the
// silent shape that made the model re-send the same patch.
func TestPatchWhitespaceNoOpRejects(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := "function up() {\n  count++;\n}\nlet count = 0;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/ws.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/ws.js", []map[string]any{
		{"search": "  count++;\n}", "replace": "  count++;\r\n}"},
	})
	if res.Success || res.Code != "patch_noop_replace" {
		t.Fatalf("a byte-identical outcome must reject as patch_noop_replace, got success=%v code=%q", res.Success, res.Code)
	}
	if v := store.ArtifactCurrentVersion(db, "file", "js/ws.js"); v != 1 {
		t.Fatalf("a no-change patch must not bump the version, got %d", v)
	}
}

// After a second consecutive anchor miss on one file, the reject escalates to a
// decisive re-save directive (the patch-loop circuit-breaker), and a
// successful write clears the streak so a later isolated miss starts fresh.
func TestPatchLoopEscalatesToResave(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	body := "export const x = 1;\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/mod.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}
	miss := []map[string]any{{"search": "ZZZ_ABSENT_ANCHOR", "replace": "ok();"}}

	// First miss: the normal message, no escalation yet.
	r1, _ := artifactActionPatch(ctx, "file", "js/mod.js", miss)
	if r1.Success || r1.Code != "patch_anchor_not_found" {
		t.Fatalf("first miss should be patch_anchor_not_found, got success=%v code=%q", r1.Success, r1.Code)
	}
	if d, _ := r1.Data.(map[string]any); d["edit_loop"] != nil {
		t.Errorf("first miss must not escalate, got edit_loop=%v", d["edit_loop"])
	}
	if strings.Contains(r1.Error, "STOP:") {
		t.Errorf("first miss must not carry the STOP escalation: %s", r1.Error)
	}

	// Second consecutive miss on the same file: escalate decisively to re-save.
	r2, _ := artifactActionPatch(ctx, "file", "js/mod.js", miss)
	d2, _ := r2.Data.(map[string]any)
	if d2 == nil || d2["edit_loop"] == nil {
		t.Fatalf("second consecutive miss must escalate (edit_loop set), got %+v", r2.Data)
	}
	if !strings.Contains(r2.Error, "STOP:") || !strings.Contains(r2.Error, "re-`save`") {
		t.Errorf("escalation must steer decisively to re-save: %s", r2.Error)
	}

	// Re-saving the whole body (the escalation's advice) clears the streak.
	if res, _ := artifactActionSaveP(ctx, "file", "js/mod.js", "export const x = 2;\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("re-save failed: %q", res.Error)
	}
	r3, _ := artifactActionPatch(ctx, "file", "js/mod.js", miss)
	if d, _ := r3.Data.(map[string]any); d["edit_loop"] != nil {
		t.Errorf("a successful save must clear the streak, but the next miss re-escalated: %v", d["edit_loop"])
	}
}

// save with attach_to performs the page link in the same call (one model round
// trip instead of save-then-attach). An attach failure is reported on the save
// result without failing the save itself.
func TestSaveWithAttachToLinksInOneCall(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	insertArtifactRow(t, db, "page", "/", "<h1>Home</h1>", "2026-06-01T14:00:00Z")

	save := &ArtSave{Kind: "file", FilePath: "css/style.css", Body: "body { margin: 0; }", AttachTo: "/"}
	res, err := save.Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("save+attach failed: %v %q", err, res.Error)
	}
	data := res.Data.(map[string]any)
	if data["attached"] == nil {
		t.Fatalf("save result must carry the attachment, got %+v", data)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_page_assets WHERE page_path = '/' AND file_path = 'css/style.css'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("link row missing: count=%d err=%v", count, err)
	}

	// Attaching to a missing page reports the failure but keeps the save.
	save2 := &ArtSave{Kind: "file", FilePath: "js/game.js", Body: "export const g = 1;", AttachTo: "/missing"}
	res2, _ := save2.Run(ctx)
	if !res2.Success {
		t.Fatal("a failed attach must not fail the save")
	}
	data2 := res2.Data.(map[string]any)
	if data2["attach_error"] == nil {
		t.Fatalf("attach failure must be reported on the save result, got %+v", data2)
	}
}

// TestArtifactGetNoReadTracker confirms get works when ReadTracker is nil (the
// chat/jobs call sites don't set one).
func TestArtifactGetNoReadTracker(t *testing.T) {
	db := setupFullArtifactsDB(t)
	insertArtifactRow(t, db, "file", "x.js", "y", "2026-06-01T14:00:00Z")
	ctx := &tools.ToolContext{DB: db} // ReadTracker nil
	res, _ := artifactActionGet(ctx, "file", "x.js", 0, 0)
	if !res.Success {
		t.Fatalf("expected success with nil ReadTracker, got %q", res.Error)
	}
}

// TestArtifactSaveInfersKind covers the save-side counterpart to
// TestArtifactGetInfersKind: a save that omits kind succeeds whenever file_path
// determines the kind on its own - a "/files/" route or static-asset extension
// (kind=file), or a leading-slash route (kind=page). Only a BARE extension-less
// name is genuinely ambiguous (layout vs component) and still asks for a kind.
func TestArtifactSaveInfersKind(t *testing.T) {
	db := setupFullArtifactsDB(t)
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}

	// /files/ route, no kind -> inferred as file, stored at the canonical path.
	res, _ := artifactActionSaveP(ctx, "", "/files/sim.world.js", "export const w = {};\n", "", "", nil, "", false)
	if !res.Success {
		t.Fatalf("save of /files/ path without kind should infer file, got %q", res.Error)
	}
	if a, err := store.ArtifactGet(db, "file", store.CanonicalFilePath("/files/sim.world.js")); err != nil || a == nil {
		t.Fatalf("expected a file artifact at the canonical path, err=%v", err)
	}

	// Static-asset extension, no /files/ prefix, no kind -> inferred as file.
	if res, _ := artifactActionSaveP(ctx, "", "styles/app.css", "body{}\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("save of an extensioned path without kind should infer file, got %q", res.Error)
	}

	// Leading-slash, extension-less route, no kind -> inferred as page. A layout or
	// component path cannot contain "/" (ValidateFilePath rejects it) and a file path
	// is stored bare, so "page" is the only kind these can legally be saved under.
	// The homepage is the case that motivated this: the mental model says "the
	// homepage is a kind=page at path /", so a kind-less save of "/" is the single
	// most predictable write a build agent makes and must resolve rather than be rejected.
	for _, p := range []string{"/", "/about", "/projects/:slug"} {
		res, _ := artifactActionSaveP(ctx, "", p, "<h1>Hi</h1>", "", "", nil, "", false)
		if !res.Success {
			t.Fatalf("save of leading-slash path %q without kind should infer page, got %q", p, res.Error)
		}
		if a, err := store.ArtifactGet(db, "page", p); err != nil || a == nil {
			t.Fatalf("expected a page artifact at %q, err=%v", p, err)
		}
	}

	// An explicit kind is never overridden by the path shape: the same leading-slash
	// path saved as kind=file must still land as a file (stored bare).
	if res, _ := artifactActionSaveP(ctx, "file", "/robots.txt", "User-agent: *\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("explicit kind=file on a leading-slash path should win, got %q", res.Error)
	}
	if a, err := store.ArtifactGet(db, "file", store.CanonicalFilePath("/robots.txt")); err != nil || a == nil {
		t.Fatalf("explicit kind=file should have stored a file artifact, err=%v", err)
	}

	// A bare extension-less name is the ONLY remaining ambiguity (layout vs
	// component), so the kind is still required - with an error that names both.
	res, _ = artifactActionSaveP(ctx, "", "sidebar", "<aside>x</aside>", "", "", nil, "", false)
	if res.Success {
		t.Fatal("save of a bare extension-less name without kind must still require kind")
	}
	if !strings.Contains(res.Error, "kind is required") {
		t.Fatalf("ambiguous create should ask for kind, got %q", res.Error)
	}
	if !strings.Contains(res.Error, "layout") || !strings.Contains(res.Error, "component") {
		t.Fatalf("the ambiguous-kind error should name both candidate kinds, got %q", res.Error)
	}
	if strings.Contains(res.Error, `kind="file"`) {
		t.Fatalf("a bare name cannot be a file; the error must not suggest kind=file, got %q", res.Error)
	}
}

// TestSaveRejectsReservedUploadsPath: the "uploads/" prefix is owned by the
// runtime upload handler (visitor uploads land there as kind=file rows and the
// upload quota is metered on it). A builder save or rename into that namespace
// could collide with a visitor upload via the of_files (kind, path) ON CONFLICT
// and skew the quota, so it is refused - across every /files/ URL form, since
// the guard runs on the canonicalized path.
func TestSaveRejectsReservedUploadsPath(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	for _, p := range []string{"uploads/app.js", "/files/uploads/app.js", "/uploads/app.js"} {
		res, _ := artifactActionSaveP(ctx, "file", p, "console.log(1)\n", "", "", nil, "", false)
		if res.Success || res.Code != "reserved_path" {
			t.Fatalf("save to %q must be refused with code reserved_path, got success=%v code=%q", p, res.Success, res.Code)
		}
	}

	// A normal asset path is unaffected.
	if res, _ := artifactActionSaveP(ctx, "file", "assets/app.js", "console.log(1)\n", "", "", nil, "", false); !res.Success {
		t.Fatalf("save to a normal path should succeed, got %q", res.Error)
	}

	// Renaming an existing code file INTO uploads/ is refused too.
	res, _ := artifactActionRename(ctx, "file", "assets/app.js", "uploads/app.js")
	if res.Success || res.Code != "reserved_path" {
		t.Fatalf("rename into uploads/ must be refused, got success=%v code=%q", res.Success, res.Code)
	}
}

// artifactActionSaveP is a positional test adapter over artifactActionSave's
// ArtSave shape, keeping the many save call sites in these tests readable.
func artifactActionSaveP(ctx *tools.ToolContext, kind, path, body, contentType, scope string, metadata map[string]any, attachTo string, overwrite bool) (*tools.Result, error) {
	return artifactActionSave(ctx, &ArtSave{Kind: kind, FilePath: path, Body: body, ContentType: contentType, Scope: scope, Metadata: metadata, AttachTo: attachTo, Overwrite: overwrite})
}
