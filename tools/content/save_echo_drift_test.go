package content

import (
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// When the platform auto-wires a missing import it PREPENDS an import line, so the
// stored body differs from what the model submitted. The save path must then behave
// like an edit: echo the changed region back with current line numbers, and NOT mark
// the whole (mutated) body as fully resident. Otherwise a re-`get` stubs, the model
// never sees the injected line, and its next patch anchors against stale content - the
// patch_anchor_not_found loop this fixes.
func TestSave_AutoWireMutation_EchoesAndDoesNotClaimFullResidence(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "util.js", `export function fmt(n){ return n + 1; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body:     `export function show(n){ return fmt(n); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("single-exporter forgotten import should auto-wire and pass, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)

	// The platform mutated the body (prepended the import), so the changed region is
	// echoed back with current line numbers, exactly as patch does.
	if echo, _ := data["updated"].(string); echo == "" {
		t.Fatalf("a body-mutating save must echo the changed region (data[\"updated\"]); got none: %+v", data)
	}

	// The whole body must NOT be claimed as fully resident: the model holds its
	// pre-prepend submission, not the stored bytes, so a repeat get must re-serve.
	got, err := store.ArtifactGet(db, "file", "page.js")
	if err != nil || got == nil {
		t.Fatalf("get page.js: %v", err)
	}
	key := tools.ReadCacheKey("file", "page.js")
	fp := tools.ReadFingerprint(got.Body)
	if ctx.ReadTracker.ShouldStubRange(key, fp, 1, 9999) {
		t.Fatalf("after an auto-wire mutation the stored body must NOT be marked fully resident (a re-get must re-serve the injected lines), but a whole-file read would be stubbed")
	}
	// The echoed region (the injected import near line 1) IS recorded resident, so a
	// re-read of just that window is still stubbed - the token saving is retained.
	if !ctx.ReadTracker.ShouldStubRange(key, fp, 1, 1) {
		t.Errorf("the echoed changed region (line 1, the injected import) should be recorded resident")
	}
}

// The mutation check is newline-agnostic: a save whose ONLY difference from the stored
// body is CRLF->LF store-normalization is NOT a mutation - it shifts no line and the
// patch matcher already normalizes newlines - so it keeps the cheap full-residence
// record and emits no echo.
func TestSave_CRLFNormalizationOnly_NotTreatedAsMutation(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "solo.js",
		Body:     "export function a(){ return 1; }\r\nexport function b(){ return 2; }",
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a self-contained CRLF module should save clean, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if echo, ok := data["updated"]; ok {
		t.Fatalf("a CRLF-only normalization is not a mutation and must NOT echo an updated region, got %v", echo)
	}

	got, err := store.ArtifactGet(db, "file", "solo.js")
	if err != nil || got == nil {
		t.Fatalf("get solo.js: %v", err)
	}
	key := tools.ReadCacheKey("file", "solo.js")
	fp := tools.ReadFingerprint(got.Body)
	if !ctx.ReadTracker.ShouldStubRange(key, fp, 1, 9999) {
		t.Fatalf("a non-mutating save must mark the body fully resident, but a whole-file read would re-serve")
	}
}

// A plain self-contained save the platform does not touch produces no echo and full
// residence. Guards against accidentally treating an untouched save as a mutation.
func TestSave_NoMutation_KeepsFullResidenceNoEcho(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "clean.js",
		Body:     "export function a(){ return 1; }\nexport function b(){ return 2; }",
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a self-contained module should save clean, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if _, ok := data["updated"]; ok {
		t.Fatalf("an untouched save must not echo an updated region, got %+v", data["updated"])
	}
	got, _ := store.ArtifactGet(db, "file", "clean.js")
	key := tools.ReadCacheKey("file", "clean.js")
	if !ctx.ReadTracker.ShouldStubRange(key, tools.ReadFingerprint(got.Body), 1, 9999) {
		t.Fatalf("an untouched save must mark the body fully resident, but a whole-file read would re-serve")
	}
}
