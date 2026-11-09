package content

import (
	"fmt"
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// TestArtSaveParsedClean pins the inline syntax status surfaced on writes so the
// build loop no longer needs a separate `check` round trip: a clean JS save
// reports parsed_clean=true; a non-JS save reports nothing; and a patch that
// repairs an already-broken file but leaves it broken reports parsed_clean=false
// with a parse_error (the write gate lets a still-broken repair through).
func TestArtSaveParsedClean(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}

	dataOf := func(res *tools.Result) map[string]any {
		t.Helper()
		m, ok := res.Data.(map[string]any)
		if !ok {
			t.Fatalf("result Data is not a map: %#v", res.Data)
		}
		return m
	}

	// Clean JS save -> parsed_clean true.
	res, err := (&ArtSave{Kind: "file", FilePath: "js/app.js", Body: "export const x = 1;\n"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("clean save: err=%v res=%+v", err, res)
	}
	if pc, ok := dataOf(res)["parsed_clean"].(bool); !ok || !pc {
		t.Errorf("clean JS save: want parsed_clean=true, got %v (ok=%v)", dataOf(res)["parsed_clean"], ok)
	}

	// Balanced CSS save -> parsed_clean true (CSS is now brace-balance gated).
	res, err = (&ArtSave{Kind: "file", FilePath: "css/main.css", Body: "body{color:red}"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("css save: err=%v res=%+v", err, res)
	}
	if pc, ok := dataOf(res)["parsed_clean"].(bool); !ok || !pc {
		t.Errorf("balanced CSS save: want parsed_clean=true, got %v (ok=%v)", dataOf(res)["parsed_clean"], ok)
	}

	// A genuinely non-gated path (HTML page) reports no parsed_clean key at all.
	res, err = (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Home</h1>"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("page save: err=%v res=%+v", err, res)
	}
	if _, present := dataOf(res)["parsed_clean"]; present {
		t.Errorf("a non-gated (HTML) save should not report parsed_clean, got %v", dataOf(res)["parsed_clean"])
	}

	// Seed an already-broken JS file directly (bypassing the save gate), then a
	// patch that changes it but leaves it broken must report parsed_clean=false.
	seedJS(t, db, "js/broken.js", "const a = [1, 2,\n")
	res, err = (&ArtPatch{Kind: "file", FilePath: "js/broken.js", Patches: []map[string]any{{"search": "const a", "replace": "const b"}}}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("repair patch: err=%v res=%+v", err, res)
	}
	if pc, ok := dataOf(res)["parsed_clean"].(bool); !ok || pc {
		t.Errorf("still-broken repair: want parsed_clean=false, got %v (ok=%v)", dataOf(res)["parsed_clean"], ok)
	}
	if _, ok := dataOf(res)["parse_error"].(string); !ok {
		t.Errorf("still-broken repair: want a parse_error string, got %v", dataOf(res)["parse_error"])
	}
}

// TestArtSaveRoundTrip pins arg forwarding for manage_files save: every
// declared arg (kind, file_path, body, content_type, scope, metadata) must
// reach the stored row. metadata is the easy one to regress: when it arrives as
// a structured object, parseMetadata must persist it rather than coercing it to
// a string and falling back to "{}". This guards that the object survives the
// round trip intact.
func TestArtSaveRoundTrip(t *testing.T) {
	db := setupBulkDB(t) // of_files + of_file_history, what ArtifactSave touches
	ctx := &tools.ToolContext{DB: db}

	act := &ArtSave{
		Kind:        "file",
		FilePath:    "css/main.css",
		Body:        "body { color: red; }",
		ContentType: "text/css; charset=utf-8",
		Scope:       "page",
		Metadata:    map[string]any{"author": "round-trip", "is_module": false},
	}
	res, err := act.Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("save: err=%v res=%+v", err, res)
	}

	a, err := store.ArtifactGet(db, "file", "css/main.css")
	if err != nil || a == nil {
		t.Fatalf("load saved artifact: err=%v a=%v", err, a)
	}
	if a.Body != "body { color: red; }" {
		t.Errorf("body not stored: %q", a.Body)
	}
	if a.ContentType != "text/css; charset=utf-8" {
		t.Errorf("content_type not stored: %q", a.ContentType)
	}
	if a.Scope != "page" {
		t.Errorf("scope not stored: %q (was the explicit top-level scope forwarded?)", a.Scope)
	}
	// The metadata object must round-trip; this is the latent drop the typed
	// handler fixes.
	if got, _ := a.Metadata["author"].(string); got != "round-trip" {
		t.Errorf("metadata.author dropped (arg-forwarding drift): %+v", a.Metadata)
	}
}

// TestArtSavePageMetadataTitleRoundTrip pins the page-title path specifically:
// a kind=page save with metadata.title must persist that title, since the old
// drop meant page <title> silently fell back to the project name.
func TestArtSavePageMetadataTitleRoundTrip(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}

	act := &ArtSave{
		Kind:     "page",
		FilePath: "/contact",
		Body:     "<h1>Contact</h1>",
		Metadata: map[string]any{"title": "Contact Us"},
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatalf("save: %v", err)
	}

	a, err := store.ArtifactGet(db, "page", "/contact")
	if err != nil || a == nil {
		t.Fatalf("load page: err=%v a=%v", err, a)
	}
	if got, _ := a.Metadata["title"].(string); got != "Contact Us" {
		t.Errorf("page metadata.title dropped: %+v", a.Metadata)
	}
}

// TestArtSaveRejectsBrokenJS pins the save-time syntax gate: a full save that
// would publish an unparseable JS module is refused and nothing is stored, so a
// broken module never reaches the snapshot or the runtime probe. A valid module
// saves normally.
func TestArtSaveRejectsBrokenJS(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "js/broken.js",
		Body:     "const a = 1;\nexport { a, missing };\n", // undeclared export -> syntax error
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("broken JS save should be refused, got %+v", res)
	}
	if res.Code != "syntax_error" {
		t.Errorf("Code = %q, want syntax_error", res.Code)
	}
	if a, _ := store.ArtifactGet(db, "file", "js/broken.js"); a != nil {
		t.Errorf("a refused save must not store the artifact, got %+v", a)
	}

	// A valid module saves normally.
	if res, err := (&ArtSave{
		Kind: "file", FilePath: "js/ok.js", Body: "export const a = 1;\n",
	}).Run(ctx); err != nil || res == nil || !res.Success {
		t.Fatalf("valid module save: err=%v res=%+v", err, res)
	}
}

// bigJS builds a parse-clean JS module well over the resave-churn min size, with
// a one-token marker so two versions can differ by a single byte.
func bigJS(marker string) string {
	var b strings.Builder
	b.WriteString("// large module\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "export const v%d = %d;\n", i, i)
	}
	fmt.Fprintf(&b, "export const MARK = '%s';\n", marker)
	return b.String()
}

// TestArtSaveIdenticalResaveIsNoOp pins the redundant-resave guard: re-saving a
// byte-identical body performs no write and reports unchanged=true with a steer
// to action=patch, so a full re-save of an unchanged file stops costing the
// output tokens of the whole body.
func TestArtSaveIdenticalResaveIsNoOp(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}
	body := "export const x = 1;\n"

	res, err := (&ArtSave{Kind: "file", FilePath: "js/a.js", Body: body}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("first save: err=%v res=%+v", err, res)
	}
	v1, _ := store.ArtifactGet(db, "file", "js/a.js")

	res, err = (&ArtSave{Kind: "file", FilePath: "js/a.js", Body: body}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("identical resave: err=%v res=%+v", err, res)
	}
	d, _ := res.Data.(map[string]any)
	if u, _ := d["unchanged"].(bool); !u {
		t.Errorf("identical resave must report unchanged=true, got %+v", d)
	}
	// No new version written.
	if v2, _ := store.ArtifactGet(db, "file", "js/a.js"); v1 != nil && v2 != nil && v1.Body != v2.Body {
		t.Errorf("identical resave must not rewrite the body")
	}
}

// TestArtSaveNearIdenticalHintsPatch pins the near-identical nudge: a full save
// that changes only a few bytes of a large file lands, but the result hints to
// use action=patch next time. A genuine rewrite gets no such hint.
func TestArtSaveNearIdenticalHintsPatch(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, err := (&ArtSave{Kind: "file", FilePath: "js/big.js", Body: bigJS("aaa")}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed big save: err=%v res=%+v", err, res)
	}

	// One-byte change in a ~2.4KB file -> hint fires.
	res, err := (&ArtSave{Kind: "file", FilePath: "js/big.js", Body: bigJS("aab")}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("near-identical resave: err=%v res=%+v", err, res)
	}
	d, _ := res.Data.(map[string]any)
	if hint, _ := d["hint"].(string); !strings.Contains(hint, "action=patch") {
		t.Errorf("near-identical full save must hint action=patch, got hint=%q", hint)
	}

	// A genuine rewrite (mostly different bytes) must NOT hint patch.
	var rewrite strings.Builder
	rewrite.WriteString("// completely different module\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&rewrite, "function fn%d() { return %d * 7; }\n", i, i)
	}
	res, err = (&ArtSave{Kind: "file", FilePath: "js/big.js", Body: rewrite.String()}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("rewrite save: err=%v res=%+v", err, res)
	}
	d, _ = res.Data.(map[string]any)
	if hint, _ := d["hint"].(string); strings.Contains(hint, "action=patch") {
		t.Errorf("a genuine rewrite must not hint patch, got hint=%q", hint)
	}
}

// TestArtPatchRoundTrip pins the patch path end to end through the typed Run.
func TestArtPatchRoundTrip(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Hello</h1>"}).Run(ctx); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	res, err := (&ArtPatch{
		Kind:     "page",
		FilePath: "/",
		Patches:  []map[string]any{{"search": "Hello", "replace": "Welcome"}},
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("patch: err=%v res=%+v", err, res)
	}

	a, err := store.ArtifactGet(db, "page", "/")
	if err != nil || a == nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(a.Body, "Welcome") {
		t.Errorf("patch not applied: %q", a.Body)
	}
}
