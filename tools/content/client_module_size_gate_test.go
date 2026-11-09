package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// oversizeJS returns a syntactically clean JS body whose length exceeds n bytes.
func oversizeJS(n int) string {
	return "const blob = \"" + strings.Repeat("x", n) + "\";\nexport const k = blob.length;\n"
}

// A client module born as one >MaxClientModuleBody blob is the shape that ships
// broken (a large single-save module hiding a forgotten import). The save gate
// must refuse it in one call so the model splits or builds it incrementally.
func TestSave_RejectsOversizedClientModule(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "huge.js",
		Body:     oversizeJS(store.MaxClientModuleBody),
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a single >MaxSingleReadBody client-JS save must be rejected, got %+v", res)
	}
	if res.Code != "module_too_large_client" {
		t.Errorf("Code = %q, want module_too_large_client", res.Code)
	}
	if a, _ := store.ArtifactGet(db, "file", "huge.js"); a != nil {
		t.Errorf("rejected save must not persist the artifact")
	}
}

// A client module comfortably under the budget saves normally - the gate only
// fires on the oversized single shot.
func TestSave_AllowsModerateClientModule(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "ok.js",
		Body:     oversizeJS(1000),
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a client-JS save under the budget must succeed, got %+v", res)
	}
}

// The cap is MaxClientModuleBody, not the MaxSingleReadBody growth-warning line: a
// JS module over the read-page budget but under the client-module cap saves in one
// shot (it only earns an advisory growth warning). Pins the cap at 36 KB, not 24.
func TestSave_AllowsClientModuleOverReadBudgetUnderCap(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := oversizeJS(store.MaxSingleReadBody + 4096) // > MaxSingleReadBody, < MaxClientModuleBody
	if len(body) <= store.MaxSingleReadBody || len(body) > store.MaxClientModuleBody {
		t.Fatalf("fixture size %d not in (%d, %d]", len(body), store.MaxSingleReadBody, store.MaxClientModuleBody)
	}
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "mid.js",
		Body:     body,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a client-JS save under the client-module cap must succeed, got %+v", res)
	}
}

// Scope check: the gate is client-JS only. A non-JS asset (CSS) over the
// read-page budget but under the single-save ceiling still saves - the import-
// coherence rationale is specific to the JS module graph.
func TestSave_AllowsOversizedNonJSAsset(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	css := "/* big */\n" + strings.Repeat(".cell{color:red}\n", 1600) // > MaxSingleReadBody, < MaxSingleSaveBody
	if len(css) <= store.MaxSingleReadBody || len(css) > store.MaxSingleSaveBody {
		t.Fatalf("fixture size %d not in (%d, %d]", len(css), store.MaxSingleReadBody, store.MaxSingleSaveBody)
	}
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "styles.css",
		Body:     css,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("an oversized non-JS asset must not be blocked by the client-JS gate, got %+v", res)
	}
}

// Never trap a corrective re-save of a file already over the budget. A module
// stored (out of band) above the line must still accept a full save - the gate
// fires only on CROSSING the line, mirroring addGrowthWarning, so a repair of an
// already-large file is never blocked.
func TestSave_DoesNotTrapAlreadyOversizedClientModule(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "already.js", oversizeJS(store.MaxClientModuleBody))

	// A modified body, still over the budget: prevBody is already over, so skip.
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "already.js",
		Body:     oversizeJS(store.MaxClientModuleBody) + "// touched\n",
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a re-save of an already-oversized module must not be trapped, got %+v", res)
	}
}
