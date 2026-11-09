package content

import (
	"fmt"
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"
)

// setupClobberDB is a migrated project database (of_page_assets included), so
// the attach_to shortcut can run end to end.
func setupClobberDB(t *testing.T) *tools.ToolContext {
	t.Helper()
	db := setupBulkDB(t)
	return &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
}

// bigGlobalCSS builds a parse-clean stylesheet well over clobberMinPrevBytes,
// standing in for a project's global design system.
func bigGlobalCSS() string {
	var b strings.Builder
	b.WriteString("/* design system */\n:root { --accent: #c00; }\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, ".token-%d { padding: %dpx; color: var(--accent); }\n", i, i)
	}
	return b.String()
}

// TestSaveClobberGateRejectsPageIntentOverGlobal pins the clobber case:
// a global design-system CSS exists, and a later save aims a tiny unrelated
// page stylesheet at the same path with attach_to. The save must be refused
// with same_path_clobber and the global file left untouched.
func TestSaveClobberGateRejectsPageIntentOverGlobal(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	global := bigGlobalCSS()

	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Home</h1>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	if res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: global, Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	res, err := (&ArtSave{
		Kind: "file", FilePath: "app.css",
		Body:     ".home-hero { display: grid; }\n.home-hero h1 { font-size: 3rem; }\n",
		AttachTo: "/",
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("page-intent save over a global file must be refused, got %+v", res)
	}
	if res.Code != "same_path_clobber" {
		t.Errorf("Code = %q, want same_path_clobber", res.Code)
	}
	if !strings.Contains(res.Error, "overwrite=true") {
		t.Errorf("rejection must name the overwrite=true escape, got %q", res.Error)
	}
	a, _ := store.ArtifactGet(db, "file", "app.css")
	if a == nil || a.Body != global {
		t.Errorf("a refused save must leave the existing global body intact")
	}
}

// TestSaveClobberGateReplaceFlagOverrides pins the deliberate escape: the same
// clobbering save with overwrite=true lands (and the attach_to shortcut still
// runs), because a from-scratch replacement is a sanctioned flow.
func TestSaveClobberGateReplaceFlagOverrides(t *testing.T) {
	ctx := setupClobberDB(t)

	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Home</h1>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	if res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: bigGlobalCSS(), Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	res, err := (&ArtSave{
		Kind: "file", FilePath: "app.css",
		Body:      ".home-hero { display: grid; }\n",
		AttachTo:  "/",
		Overwrite: true,
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("overwrite=true save must land: err=%v res=%+v", err, res)
	}
	d, _ := res.Data.(map[string]any)
	if d["attached"] == nil {
		t.Errorf("attach_to must still run on a overwrite=true save, got %+v", d)
	}
}

// TestSaveClobberGateIdenticalResaveStillAttaches pins the legitimate
// scope-fix flow: re-sending the SAME global body with attach_to is a no-op
// save that still attaches (byte-identical short-circuit runs before the gate).
func TestSaveClobberGateIdenticalResaveStillAttaches(t *testing.T) {
	ctx := setupClobberDB(t)
	global := bigGlobalCSS()

	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Home</h1>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	if res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: global, Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: global, AttachTo: "/"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("identical resave: err=%v res=%+v", err, res)
	}
	d, _ := res.Data.(map[string]any)
	if u, _ := d["unchanged"].(bool); !u {
		t.Errorf("identical resave must report unchanged=true, got %+v", d)
	}
	if d["attached"] == nil {
		t.Errorf("identical resave with attach_to must still attach, got %+v", d)
	}
}

// TestSaveClobberGateAllowsGenuineRewrite pins the build-frame rewrite flow: a
// full from-scratch rewrite of a global file at a similar size (no shrink, no
// page intent) is NOT a clobber and must land without overwrite=true.
func TestSaveClobberGateAllowsGenuineRewrite(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	if res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: bigGlobalCSS(), Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	var rewrite strings.Builder
	rewrite.WriteString("/* redesigned system */\n:root { --ink: #222; }\n")
	for i := 0; i < 110; i++ {
		fmt.Fprintf(&rewrite, ".surface-%d { margin: %dpx; border-radius: 8px; }\n", i, i)
	}
	res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: rewrite.String()}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("similar-size rewrite must land without overwrite=true: err=%v res=%+v", err, res)
	}
}

// TestSaveClobberGateIgnoresNewAndPageScopedFiles pins the gate's scoping: a
// fresh save with attach_to (no existing file) and a re-save of an
// already-page-scoped file are both outside the gate.
func TestSaveClobberGateIgnoresNewAndPageScopedFiles(t *testing.T) {
	ctx := setupClobberDB(t)

	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Home</h1>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	// New file + attach_to.
	if res, err := (&ArtSave{Kind: "file", FilePath: "css/home.css", Body: ".hero { color: red; }\n", AttachTo: "/"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("new file with attach_to must land: err=%v res=%+v", err, res)
	}
	// Re-save of a page-scoped file with a different tiny body.
	if res, err := (&ArtSave{Kind: "file", FilePath: "css/home.css", Body: ".hero { color: blue; }\n", AttachTo: "/"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("page-scoped re-save must land: err=%v res=%+v", err, res)
	}
}

// TestSaveClobberGateRejectsBlindDestructiveShrink pins the two-call variant,
// where the save carries no page-intent signal (the attach comes later) but
// replaces a large global file with a tiny unrelated body.
func TestSaveClobberGateRejectsBlindDestructiveShrink(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	global := bigGlobalCSS()

	if res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: global, Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	res, err := (&ArtSave{
		Kind: "file", FilePath: "app.css",
		Body: ".page-only { display: flex; }\n",
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("blind destructive shrink over a global file must be refused, got %+v", res)
	}
	if res.Code != "same_path_clobber" {
		t.Errorf("Code = %q, want same_path_clobber", res.Code)
	}
	if a, _ := store.ArtifactGet(db, "file", "app.css"); a == nil || a.Body != global {
		t.Errorf("a refused save must leave the existing global body intact")
	}
}
