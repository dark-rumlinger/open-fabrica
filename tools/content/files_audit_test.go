package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"
)

func auditCtx(t *testing.T) *tools.ToolContext {
	t.Helper()
	return &tools.ToolContext{DB: setupBulkDB(t), ReadTracker: tools.NewReadTracker()}
}

func markPlatformManaged(t *testing.T, ctx *tools.ToolContext, kind, path, body string) {
	t.Helper()
	if _, _, err := store.ArtifactSave(ctx.DB, &store.Artifact{
		Kind: kind, Path: path, Storage: store.StorageInline, Body: body,
		ContentType: "application/javascript", Scope: "global", Status: "published",
		Metadata: map[string]any{"platform_managed": true},
	}, "platform"); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// Every writer, not only delete, refuses a platform-managed helper.
func TestPlatformManagedRefusedByEveryWriter(t *testing.T) {
	ctx := auditCtx(t)
	markPlatformManaged(t, ctx, "file", "js/auth.js", "export const x = 1;\n")

	if res, _ := artifactActionPatch(ctx, "file", "js/auth.js", []map[string]any{{"search": "x = 1", "replace": "x = 2"}}); res == nil || res.Success {
		t.Fatalf("patch must be refused, got %+v", res)
	}
	if res, _ := artifactActionRename(ctx, "file", "js/auth.js", "js/other.js"); res == nil || res.Success {
		t.Fatalf("rename must be refused, got %+v", res)
	}
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "file", FilePath: "js/auth.js", Body: "export const x = 3;\n"}); res == nil || res.Success {
		t.Fatalf("save must be refused, got %+v", res)
	}
	res, _ := artifactActionBulkReplace(ctx, &ArtBulkReplace{Kind: "file", Pattern: "x = 1", Replace: "x = 9"})
	if res == nil || !res.Success {
		t.Fatalf("bulk_replace should report, got %+v", res)
	}
	if a, _ := store.ArtifactGet(ctx.DB, "file", "js/auth.js"); a == nil || !strings.Contains(a.Body, "x = 1") {
		t.Fatalf("platform file must be untouched, got %+v", a)
	}
}

// bulk_replace path_prefix is literal: _ is not a wildcard, case matters, and the
// /files/ URL form is canonicalized.
func TestBulkReplacePrefixIsLiteral(t *testing.T) {
	ctx := auditCtx(t)
	seedJS(t, ctx.DB, "js_a/x.js", "export const v = 'old';\n")
	seedJS(t, ctx.DB, "jsXa/y.js", "export const v = 'old';\n")
	seedJS(t, ctx.DB, "JS_A/z.js", "export const v = 'old';\n")
	res, _ := artifactActionBulkReplace(ctx, &ArtBulkReplace{Kind: "file", PathPrefix: "/files/js_a/", Pattern: "old", Replace: "new"})
	if res == nil || !res.Success {
		t.Fatalf("got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if data["files_changed"] != 1 {
		t.Fatalf("only js_a/x.js should change, got %+v", data)
	}
	for _, p := range []string{"jsXa/y.js", "JS_A/z.js"} {
		if a, _ := store.ArtifactGet(ctx.DB, "file", p); a == nil || !strings.Contains(a.Body, "old") {
			t.Fatalf("%s must be untouched", p)
		}
	}
}

// An invalid regex is reported even when no artifact matches the scope.
func TestBulkReplaceInvalidRegexWithNoCandidates(t *testing.T) {
	ctx := auditCtx(t)
	res, _ := artifactActionBulkReplace(ctx, &ArtBulkReplace{Kind: "file", Pattern: "(", Replace: "x"})
	if res == nil || res.Success {
		t.Fatalf("an invalid regex must fail, got %+v", res)
	}
}

// A re-save without metadata keeps the stored metadata and content type.
func TestResaveKeepsMetadata(t *testing.T) {
	ctx := auditCtx(t)
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "page", FilePath: "/contact", Body: "<h1>a</h1>", Metadata: map[string]any{"title": "Contact", "layout": "alt"}}); res == nil || !res.Success {
		t.Fatalf("first save: %+v", res)
	}
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "page", FilePath: "/contact", Body: "<h1>b</h1>"}); res == nil || !res.Success {
		t.Fatalf("second save: %+v", res)
	}
	a, _ := store.ArtifactGet(ctx.DB, "page", "/contact")
	if a == nil || a.Metadata["title"] != "Contact" || a.Metadata["layout"] != "alt" {
		t.Fatalf("metadata lost on re-save: %+v", a)
	}
}

// A same-body save that restates scope is a real edit, not an "unchanged" no-op.
func TestIdenticalBodyResaveStillAppliesScope(t *testing.T) {
	ctx := auditCtx(t)
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "file", FilePath: "css/a.css", Body: "a{}\n", Scope: "page"}); res == nil || !res.Success {
		t.Fatalf("first save: %+v", res)
	}
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "file", FilePath: "css/a.css", Body: "a{}\n", Scope: "global"}); res == nil || !res.Success {
		t.Fatalf("second save: %+v", res)
	}
	if a, _ := store.ArtifactGet(ctx.DB, "file", "css/a.css"); a == nil || a.Scope != "global" {
		t.Fatalf("scope not applied: %+v", a)
	}
}

// Text extensions outside the old content-type table keep their body.
func TestSaveKeepsBodyForMarkdown(t *testing.T) {
	ctx := auditCtx(t)
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "file", FilePath: "docs/readme.md", Body: "# hi\n"}); res == nil || !res.Success {
		t.Fatalf("save: %+v", res)
	}
	if a, _ := store.ArtifactGet(ctx.DB, "file", "docs/readme.md"); a == nil || a.Body != "# hi\n" {
		t.Fatalf("body lost: %+v", a)
	}
}

// Rename validates its target, reuses a deleted path, and keeps history with the file.
func TestRenameTargetValidationTombstoneAndHistory(t *testing.T) {
	ctx := auditCtx(t)
	seedJS(t, ctx.DB, "js/old.js", "export const a = 1;\n")
	seedJS(t, ctx.DB, "js/user.js", "import { a } from 'old';\nexport const b = a;\n")

	if res, _ := artifactActionRename(ctx, "file", "js/old.js", "js/auth.js"); res == nil || res.Success {
		t.Fatalf("a rename onto a reserved helper name must be refused, got %+v", res)
	}
	seedJS(t, ctx.DB, "js/gone.js", "export const g = 1;\n")
	if ok, err := store.ArtifactDelete(ctx.DB, "file", "js/gone.js", "test"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	res, _ := artifactActionRename(ctx, "file", "js/old.js", "js/gone.js")
	if res == nil || !res.Success {
		t.Fatalf("a rename onto a deleted path must succeed, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if note, _ := data["importer_note"].(string); !strings.Contains(note, "js/user.js") {
		t.Fatalf("importers of the old name must be flagged, got %+v", data)
	}
	if v := store.ArtifactCurrentVersion(ctx.DB, "file", "js/gone.js"); v < 1 {
		t.Fatalf("history must follow the renamed file, version %d", v)
	}
}

// restore of a deleted artifact brings back its metadata, and the flow script cannot
// be deleted or renamed through manage_files.
func TestRestoreKeepsMetadataAndFlowScriptGuarded(t *testing.T) {
	ctx := auditCtx(t)
	if res, _ := artifactActionSave(ctx, &ArtSave{Kind: "page", FilePath: "/p", Body: "<p>x</p>", Metadata: map[string]any{"title": "P"}}); res == nil || !res.Success {
		t.Fatalf("save: %+v", res)
	}
	if res, _ := artifactActionDelete(ctx, "page", "/p"); res == nil || !res.Success {
		t.Fatalf("delete: %+v", res)
	}
	if res, _ := artifactActionRestore(ctx, "page", "/p", 0); res == nil || !res.Success {
		t.Fatalf("restore: %+v", res)
	}
	if a, _ := store.ArtifactGet(ctx.DB, "page", "/p"); a == nil || a.Metadata["title"] != "P" {
		t.Fatalf("metadata lost across delete + restore: %+v", a)
	}
	if res, _ := artifactActionDelete(ctx, "test", "of_test.js"); res == nil || res.Success {
		t.Fatalf("deleting the flow script must be refused, got %+v", res)
	}
}

// A forgotten capitalized sibling class is auto-wired like a lowercase helper.
func TestCapitalizedSiblingClassIsAutoWired(t *testing.T) {
	ctx := auditCtx(t)
	seedJS(t, ctx.DB, "widget.js", "export class Widget {}\n")
	seedJS(t, ctx.DB, "page.js", "export function show(){ return 1; }\n")
	res, _ := artifactActionPatch(ctx, "file", "page.js", []map[string]any{{"search": "return 1;", "replace": "return new Widget();"}})
	if res == nil || !res.Success {
		t.Fatalf("patch: %+v", res)
	}
	if a, _ := store.ArtifactGet(ctx.DB, "file", "page.js"); a == nil || !strings.Contains(a.Body, "import { Widget }") {
		t.Fatalf("Widget import not wired: %+v", a)
	}
}
