package content

// A page-scope file that no page loads is reported by the NEXT turn's
// project_state, which is a turn too late to fold the fix into the save that
// caused it. The save result names the gap instead, and must name it correctly
// for every project shape: an entry needs the link, a leaf reached through the
// import graph must not be attached, a project with no pages cannot attach
// anything, and a project with no page-scope assets never sees the advice.

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

func saveNote(t *testing.T, ctx *tools.ToolContext, a *ArtSave) string {
	t.Helper()
	res, err := a.Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("save %s: err=%v res=%+v", a.FilePath, err, res)
	}
	data, _ := res.Data.(map[string]any)
	note, _ := data["link_note"].(string)
	return note
}

// An entry saved without attach_to is the case that costs a turn.
func TestUnlinkedSaveNoteOnEntry(t *testing.T) {
	ctx := setupClobberDB(t)
	mustSavePage(t, ctx, "/gallery")

	note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "pages/gallery.js", Scope: "page",
		Body: "export function boot(){}\n"})
	if note == "" {
		t.Fatal("an entry that no page loads got no note; the model only learns from project_state a turn later")
	}
	if !strings.Contains(note, "attach_to") {
		t.Errorf("the note must name the arg that fixes it in one call, got: %s", note)
	}
	if !strings.Contains(note, "leaf") {
		t.Errorf("the note must exempt leaf modules, or it reads as attach-everything, got: %s", note)
	}
}

// The same save carrying attach_to is already linked, so there is nothing to say.
func TestUnlinkedSaveNoteSilentWhenAttached(t *testing.T) {
	ctx := setupClobberDB(t)
	mustSavePage(t, ctx, "/gallery")

	if note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "pages/gallery.js", Scope: "page",
		Body: "export function boot(){}\n", AttachTo: "/gallery"}); note != "" {
		t.Errorf("a save that linked itself must not be told to link itself, got: %s", note)
	}
}

// A leaf the linked entry imports is reachable, so attaching it would be wrong.
// This is the case a path-shaped rule would get backwards.
func TestUnlinkedSaveNoteSilentForImportedLeaf(t *testing.T) {
	ctx := setupClobberDB(t)
	mustSavePage(t, ctx, "/gallery")
	if note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "pages/state.js", Scope: "page",
		Body: "export const state = {};\n"}); note == "" {
		t.Fatal("precondition: a leaf saved before its entry is genuinely orphaned and should be noted")
	}
	// The entry lands linked and imports the leaf, which now rides the graph. An
	// attached entry is the graph's unimported root, so it carries no exports of
	// its own (the entry_exports gate refuses that).
	saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "pages/gallery.js", Scope: "page",
		Body:     "import { state } from './state.js';\ndocument.title = String(state.v);\n",
		AttachTo: "/gallery"})

	if note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "pages/state.js", Scope: "page",
		Body: "export const state = { v: 1 };\n"}); note != "" {
		t.Errorf("a leaf reached from a linked entry must NOT be told to attach - the import graph "+
			"already serves it, and attaching leaves is the wiring mistake the graph exists to avoid. got: %s", note)
	}
}

// CSS can never be reached by an import, so it always needs its own link.
func TestUnlinkedSaveNoteOnPageCSS(t *testing.T) {
	ctx := setupClobberDB(t)
	mustSavePage(t, ctx, "/gallery")

	if note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "css/gallery.css", Scope: "page",
		Body: ".hero{color:red}\n"}); note == "" {
		t.Error("page CSS cannot be imported by a module, so an unlinked stylesheet must be reported")
	}
}

// A headless project has no page to attach to; telling it to pass attach_to
// would send it looking for a route that does not exist.
func TestUnlinkedSaveNoteWithoutAnyPage(t *testing.T) {
	ctx := setupClobberDB(t)

	note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "lib/util.js", Scope: "page",
		Body: "export const x = 1;\n"})
	if note == "" {
		t.Fatal("a page-scope file in a project with no pages can never load and must be reported")
	}
	if strings.Contains(note, "attach_to") {
		t.Errorf("with no pages there is no route to attach to; the note must not send it looking, got: %s", note)
	}
	if !strings.Contains(note, "global") {
		t.Errorf("the note should offer the scope that works without a page, got: %s", note)
	}
}

// Nothing page-scope, nothing to say: a global asset and a non-page project
// never reach the advice.
func TestUnlinkedSaveNoteInertForGlobalAsset(t *testing.T) {
	ctx := setupClobberDB(t)
	mustSavePage(t, ctx, "/gallery")

	if note := saveNote(t, ctx, &ArtSave{Kind: "file", FilePath: "site.css", Scope: "global",
		Body: ".btn{color:red}\n"}); note != "" {
		t.Errorf("a sitewide asset loads on every page and needs no attach, got: %s", note)
	}
}

// A page save is not attachable at all and must never carry the note.
func TestUnlinkedSaveNoteSkipsNonFileKinds(t *testing.T) {
	ctx := setupClobberDB(t)
	if note := saveNote(t, ctx, &ArtSave{Kind: "page", FilePath: "/about", Body: "<h1>About</h1>"}); note != "" {
		t.Errorf("a page is not attachable, got: %s", note)
	}
}

func mustSavePage(t *testing.T, ctx *tools.ToolContext, route string) {
	t.Helper()
	if res, err := (&ArtSave{Kind: "page", FilePath: route, Body: "<h1>hi</h1>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page %s: err=%v res=%+v", route, err, res)
	}
}
