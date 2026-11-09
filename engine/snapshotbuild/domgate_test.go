package snapshotbuild

// A page's DOM contract must be readable WHILE the modules that bind to it are
// being written - not only once an entry is attached: platform guidance is to
// save the modules first and attach ONLY the entry at the end, so a link-keyed
// gate would withhold the contract for the whole build. That matters because a
// shared `dom` object is conventionally keyed BY the page's element ids, so
// without the contract a leaf invents names - binding `dom.mini` against a
// declared `minimap` - which passes every static check and only misbehaves at
// runtime behind an interaction the load probe never triggers.

import (
	"context"
	"strings"
	"testing"
)

// The mid-split state: page-scope modules saved, no entry attached yet. This is where a
// build spends most of its life, and where the ids are needed.
func TestSnapshot_DOMContractBeforeEntryIsLinked(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('page','/','','<div id="mount"></div><div id="nametags"></div><canvas id="minimap"></canvas><span id="counterText"></span>','text/html')`)
	// Saved, not yet attached - exactly what the platform tells the model to do.
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','game.state.js','page','export const dom = {};','application/javascript')`)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','game.chat.js','page','export function initChat(){}','application/javascript')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.PageDOM) != 1 {
		t.Fatalf("no DOM contract for a page with unlinked page-scope JS (got %d).\n"+
			"The modules that bind these ids are being written NOW; handing the contract over "+
			"only once the entry is attached delivers it after every leaf is already wrong.", len(s.PageDOM))
	}
	got := strings.Join(s.PageDOM[0].IDs, ",")
	for _, id := range []string{"mount", "nametags", "minimap", "counterText"} {
		if !strings.Contains(got, id) {
			t.Errorf("id %q missing from the page's DOM contract (got %s)", id, got)
		}
	}
	if !strings.Contains(s.Render(), "### Page DOM anchors") {
		t.Error("Render() omits the Page DOM anchors section while page-scope JS is unlinked - " +
			"the guide tells the model to read that section every turn, so its absence is what forces a guess")
	}
}

// The optimisation the gate exists for must survive: a project with no page-scope JS has
// no DOM to wire, so listing ids is noise.
func TestSnapshot_NoDOMContractWithoutPageScopeJS(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('page','/about','','<h1 id="title">hi</h1>','text/html')`)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','site.css','global','.a{}','text/css')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.PageDOM) != 0 {
		t.Errorf("a project with no page-scope JS must get no DOM contract, got %d", len(s.PageDOM))
	}
	if strings.Contains(s.Render(), "Page DOM anchors") {
		t.Error("Page DOM anchors rendered for a project with nothing to bind them")
	}
}

// Once a link exists it is the better signal, and the fallback must stop widening: a
// page with no script of its own should not inherit another page's DOM contract.
func TestSnapshot_LinkedEntryNarrowsTheDOMContract(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('page','/','','<div id="mount"></div>','text/html')`)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('page','/about','','<h1 id="title">hi</h1>','text/html')`)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','game.js','page','export const dom = {};','application/javascript')`)
	mustExec(t, db, `INSERT INTO of_page_assets (page_path, file_path) VALUES ('/','game.js')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.PageDOM) != 1 {
		t.Fatalf("expected exactly the linked page's DOM contract, got %d", len(s.PageDOM))
	}
	if s.PageDOM[0].Path != "/" {
		t.Errorf("DOM contract keyed on %q, want \"/\" - the fallback must yield to a real link", s.PageDOM[0].Path)
	}
}
