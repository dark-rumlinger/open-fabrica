package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// TestEditActionsInferKind pins the ergonomics fix: patch operates only on an
// artifact that already exists, so it infers kind from file_path (like the
// read actions) instead of forcing a weaker model to re-supply the kind it
// already gave on save. This avoids spurious "missing required arg kind"
// rejections on edit actions.
func TestEditActionsInferKind(t *testing.T) {
	t.Run("patch infers kind from path", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		if _, err := (&ArtSave{Kind: "file", FilePath: "js/room.js", Body: "const a = 1;\n"}).Run(ctx); err != nil {
			t.Fatalf("seed save: %v", err)
		}
		// kind intentionally omitted.
		res, err := (&ArtPatch{
			FilePath: "js/room.js",
			Patches:  []map[string]any{{"search": "const a = 1;", "replace": "const a = 2;"}},
		}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("patch without kind should infer and succeed: err=%v res=%+v", err, res)
		}
		a, _ := store.ArtifactGet(db, "file", "js/room.js")
		if a == nil || !strings.Contains(a.Body, "const a = 2;") {
			t.Fatalf("patch not applied: %+v", a)
		}
	})

	t.Run("ambiguous path asks to disambiguate", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		// Seed the SAME bare-name path under two distinct kinds. Both kinds must
		// be ones that genuinely accept an extension-less name, or only one row
		// is created and the path is no longer ambiguous: a "page" path is
		// rejected by validatePagePath (must start with "/"), so seeding
		// page+component would silently leave just the component row and let
		// inference resolve it - the false-pass this test guards against.
		if r, err := (&ArtSave{Kind: "component", FilePath: "shared", Body: "a\nb\n"}).Run(ctx); err != nil || r == nil || !r.Success {
			t.Fatalf("seed component: err=%v res=%+v", err, r)
		}
		if r, err := (&ArtSave{Kind: "layout", FilePath: "shared", Body: "<div>{{content}}</div>"}).Run(ctx); err != nil || r == nil || !r.Success {
			t.Fatalf("seed layout: err=%v res=%+v", err, r)
		}
		// patch with kind omitted must refuse rather than silently pick one.
		res, err := (&ArtPatch{
			FilePath: "shared",
			Patches:  []map[string]any{{"search": "a", "replace": "z"}},
		}).Run(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil || res.Success || res.Code != "ambiguous_kind" {
			t.Fatalf("expected ambiguous_kind failure, got success=%v code=%q res=%+v", res.Success, res.Code, res)
		}
		if !strings.Contains(res.Error, "component") || !strings.Contains(res.Error, "layout") {
			t.Fatalf("error should name the candidate kinds, got %q", res.Error)
		}
	})

	t.Run("missing path without kind reports not-found, not missing-arg", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		res, err := (&ArtPatch{
			FilePath: "js/nope.js",
			Patches:  []map[string]any{{"search": "a", "replace": "b"}},
		}).Run(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil || res.Success {
			t.Fatalf("expected failure for missing artifact, got %+v", res)
		}
		if strings.Contains(strings.ToLower(res.Error), "required") {
			t.Fatalf("should not be a missing-arg error anymore: %q", res.Error)
		}
	})
}

// TestLifecycleActionsInferKind extends the same ergonomics fix to delete and
// rename: both target an artifact that already exists, so kind is inferred from
// file_path via resolveKindPath instead of failing with missing_arg and costing
// a retry turn. restore is deliberately excluded - inference only sees live
// rows, and restore must reach a just-deleted tombstone.
func TestLifecycleActionsInferKind(t *testing.T) {
	t.Run("delete infers kind from path", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		if _, err := (&ArtSave{Kind: "file", FilePath: "js/stray.js", Body: "const a = 1;\n"}).Run(ctx); err != nil {
			t.Fatalf("seed save: %v", err)
		}
		// kind intentionally omitted.
		res, err := (&ArtDelete{FilePath: "js/stray.js"}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("delete without kind should infer and succeed: err=%v res=%+v", err, res)
		}
		if a, _ := store.ArtifactGet(db, "file", "js/stray.js"); a != nil {
			t.Fatalf("artifact should be deleted, still live: %+v", a)
		}
	})

	t.Run("rename infers source kind from path", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		if _, err := (&ArtSave{Kind: "file", FilePath: "js/app.js", Body: "const a = 1;\n"}).Run(ctx); err != nil {
			t.Fatalf("seed save: %v", err)
		}
		res, err := (&ArtRename{FilePath: "js/app.js", NewPath: "js/main.js"}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("rename without kind should infer and succeed: err=%v res=%+v", err, res)
		}
		if a, _ := store.ArtifactGet(db, "file", "js/app.js"); a != nil {
			t.Fatalf("source path should be gone after rename: %+v", a)
		}
		if a, _ := store.ArtifactGet(db, "file", "js/main.js"); a == nil {
			t.Fatal("renamed artifact not found at destination")
		}
	})

	t.Run("ambiguous path asks to disambiguate instead of deleting", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		// Same bare-name path under two kinds that both accept extension-less
		// names (see TestEditActionsInferKind for why layout+component and not
		// page+anything).
		if r, err := (&ArtSave{Kind: "component", FilePath: "shared", Body: "a\nb\n"}).Run(ctx); err != nil || r == nil || !r.Success {
			t.Fatalf("seed component: err=%v res=%+v", err, r)
		}
		if r, err := (&ArtSave{Kind: "layout", FilePath: "shared", Body: "<div>{{content}}</div>"}).Run(ctx); err != nil || r == nil || !r.Success {
			t.Fatalf("seed layout: err=%v res=%+v", err, r)
		}
		res, err := (&ArtDelete{FilePath: "shared"}).Run(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil || res.Success || res.Code != "ambiguous_kind" {
			t.Fatalf("expected ambiguous_kind failure, got success=%v code=%q res=%+v", res.Success, res.Code, res)
		}
		// Neither kind may have been deleted by the refused call.
		if a, _ := store.ArtifactGet(db, "component", "shared"); a == nil {
			t.Fatal("component should survive an ambiguous delete refusal")
		}
		if a, _ := store.ArtifactGet(db, "layout", "shared"); a == nil {
			t.Fatal("layout should survive an ambiguous delete refusal")
		}
	})

	t.Run("missing path without kind reports not-found, not missing-arg", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		res, err := (&ArtDelete{FilePath: "js/nope.js"}).Run(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil || res.Success {
			t.Fatalf("expected failure for missing artifact, got %+v", res)
		}
		if strings.Contains(strings.ToLower(res.Error), "required") {
			t.Fatalf("should not be a missing-arg error anymore: %q", res.Error)
		}
	})
}

// TestEditActionsAcceptURLFormPath pins the save/edit path symmetry: save accepts
// "/js/x.js" (kind inferred from the extension, leading slash canonicalized away),
// so a kind-less patch/get/delete on the SAME string must resolve to the stored
// row instead of failing not-found. The retry is by lookup, not a forced
// kind=file, so an extensioned artifact of another kind keeps its own kind.
func TestEditActionsAcceptURLFormPath(t *testing.T) {
	seed := func(t *testing.T) (*tools.ToolContext, string) {
		t.Helper()
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		// kind intentionally omitted on save too: the kind-less save-then-read sequence.
		if r, err := (&ArtSave{FilePath: "/js/messages.js", Body: "const a = 1;\n"}).Run(ctx); err != nil || r == nil || !r.Success {
			t.Fatalf("seed save: err=%v res=%+v", err, r)
		}
		return ctx, "/js/messages.js"
	}

	t.Run("patch accepts the leading-slash form used on save", func(t *testing.T) {
		ctx, urlPath := seed(t)
		res, err := (&ArtPatch{
			FilePath: urlPath,
			Patches:  []map[string]any{{"search": "const a = 1;", "replace": "const a = 2;"}},
		}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("patch on %q should resolve the stored row: err=%v res=%+v", urlPath, err, res)
		}
		a, _ := store.ArtifactGet(ctx.DB, "file", "js/messages.js")
		if a == nil || !strings.Contains(a.Body, "const a = 2;") {
			t.Fatalf("patch not applied to canonical row: %+v", a)
		}
	})

	t.Run("get accepts the leading-slash form", func(t *testing.T) {
		ctx, urlPath := seed(t)
		res, err := (&ArtGet{FilePath: urlPath}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("get on %q should succeed: err=%v res=%+v", urlPath, err, res)
		}
	})

	t.Run("delete accepts the leading-slash form", func(t *testing.T) {
		ctx, urlPath := seed(t)
		res, err := (&ArtDelete{FilePath: urlPath}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("delete on %q should succeed: err=%v res=%+v", urlPath, err, res)
		}
		if a, _ := store.ArtifactGet(ctx.DB, "file", "js/messages.js"); a != nil {
			t.Fatalf("canonical row should be deleted, still live: %+v", a)
		}
	})

	t.Run("retry resolves an extensioned non-file kind, not forced file", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		if r, err := (&ArtSave{Kind: "component", FilePath: "card.html", Body: "<div>a</div>"}).Run(ctx); err != nil || r == nil || !r.Success {
			t.Fatalf("seed component: err=%v res=%+v", err, r)
		}
		res, err := (&ArtPatch{
			FilePath: "/card.html",
			Patches:  []map[string]any{{"search": "<div>a</div>", "replace": "<div>b</div>"}},
		}).Run(ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("patch on /card.html should resolve the component: err=%v res=%+v", err, res)
		}
		a, _ := store.ArtifactGet(db, "component", "card.html")
		if a == nil || !strings.Contains(a.Body, "<div>b</div>") {
			t.Fatalf("component not patched: %+v", a)
		}
	})

	t.Run("genuinely missing url-form path still reports not-found", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db}
		res, err := (&ArtPatch{
			FilePath: "/js/nope.js",
			Patches:  []map[string]any{{"search": "a", "replace": "b"}},
		}).Run(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil || res.Success {
			t.Fatalf("expected not-found failure, got %+v", res)
		}
	})
}
