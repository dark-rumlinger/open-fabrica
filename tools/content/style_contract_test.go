package content

import (
	"strings"
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// designSystemCSS defines a handful of classes (including a compound `.act.liked`,
// which must count `liked` as defined) so the contract check has a real design
// system to judge against.
const designSystemCSS = `.pill-btn { color: #fff; }
.act { background: none; }
.act.liked { color: #f91880; }
.card { border: 1px solid; }
.nav-item { padding: 8px; }
.avatar { border-radius: 50%; }`

func saveDesignSystem(t *testing.T, db *tools.ToolContext) {
	t.Helper()
	res, err := (&ArtSave{Kind: "file", FilePath: "css/global.css", Body: designSystemCSS}).Run(db)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("seed CSS: err=%v res=%+v", err, res)
	}
}

// A save whose markup uses a class no stylesheet defines succeeds but carries a
// non-blocking advisory naming the undefined class - and never flags a class the
// design system does define, including one reachable only through a compound
// selector.
func TestSave_FlagsUndefinedClassNonBlocking(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	saveDesignSystem(t, ctx)

	res, err := (&ArtSave{
		Kind:     "component",
		FilePath: "post-card",
		Body:     `<article class="card"><button class="act liked">x</button><button class="mystery-btn">y</button></article>`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("the save must SUCCEED (advisory is non-blocking), got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	note, _ := data["style_notes"].(string)
	if note == "" {
		t.Fatal("expected a style_notes advisory for the undefined class")
	}
	if !strings.Contains(note, "mystery-btn") {
		t.Errorf("advisory should name the undefined class, got %q", note)
	}
	for _, defined := range []string{"card", "act", "liked"} {
		if strings.Contains(note, defined) {
			t.Errorf("advisory must not flag defined class %q, got %q", defined, note)
		}
	}
}

// A hook/behavior class with no CSS rule, riding on an element that IS styled by
// a sibling class (`act a-like`), must NOT be flagged - the element is styled, and
// the hook class is intentional. This is the dominant real-world false-positive
// mode, so it is pinned.
func TestSave_IgnoresHookClassOnStyledElement(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	saveDesignSystem(t, ctx)

	res, err := (&ArtSave{
		Kind:     "component",
		FilePath: "actions",
		Body:     `<button class="act a-like">L</button><button class="act a-reply">R</button>`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if data, _ := res.Data.(map[string]any); data["style_notes"] != nil {
		t.Errorf("a hook class beside a styled class must not be flagged, got %v", data["style_notes"])
	}
}

// Markup that uses only defined classes gets no advisory.
func TestSave_NoNoteWhenAllClassesDefined(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	saveDesignSystem(t, ctx)

	res, err := (&ArtSave{
		Kind:     "component",
		FilePath: "ok-card",
		Body:     `<div class="card"><span class="avatar"></span><button class="pill-btn">go</button></div>`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if data, _ := res.Data.(map[string]any); data["style_notes"] != nil {
		t.Errorf("no advisory expected when every class is defined, got %v", data["style_notes"])
	}
}

// A project without a real design system (few or no defined classes) stays silent,
// so utility-framework or pre-CSS projects are not spammed.
func TestSave_SilentWithoutDesignSystem(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "component",
		FilePath: "loose",
		Body:     `<button class="whatever-btn other-thing">z</button>`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if data, _ := res.Data.(map[string]any); data["style_notes"] != nil {
		t.Errorf("no design system -> no advisory, got %v", data["style_notes"])
	}
}
