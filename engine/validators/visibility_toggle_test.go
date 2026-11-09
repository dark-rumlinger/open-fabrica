package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setupToggleDB builds the of_files slice the visibility-toggle validator reads:
// like setupPageDB but with the metadata column the scan queries (COALESCE'd).
func setupToggleDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT DEFAULT '', scope TEXT,
		metadata TEXT DEFAULT '', is_deleted INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_files: %v", err)
	}
	return db
}

func toggleIssues(t *testing.T, db *sql.DB) []Issue {
	t.Helper()
	return visibilityToggleValidator{}.Validate(context.Background(), db, "", "")
}

// A panel hidden by `.hidden{display:none!important}`, whose
// open button toggles `open` - a class with no rule that shows #sidebar (the only
// `.open` rule targets a different element) and that never removes `hidden`. The
// handler fires; nothing appears. Must flag exactly #sidebar.
func TestVisibilityToggle_FlagsHiddenOpenMismatch(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css",
		`.hidden{display:none!important} .sidebar{position:fixed} #rightPanel.open{transform:none}`)
	saveKind(t, db, "page", "/",
		`<button id="btnProjects">Projects</button>
		 <div id="sidebar" class="panel sidebar hidden">projects</div>`)
	saveKind(t, db, "file", "app.js",
		`const on = id => document.getElementById(id);
		 on('btnProjects')?.addEventListener('click', () => on('sidebar')?.classList.toggle('open'));`)

	issues := toggleIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("expected 1 dead_toggle issue, got %d: %+v", len(issues), issues)
	}
	is := issues[0]
	if is.Category != "dead_toggle" || is.Page != "/" || is.Signature != "sidebar" {
		t.Errorf("unexpected issue: %+v", is)
	}
	if is.Severity != "error" || !is.Deterministic {
		t.Errorf("dead_toggle must be a deterministic error: %+v", is)
	}
}

// The reveal goes through a bound variable
// (`const drawer = getElementById('templates-menu'); drawer.classList.toggle('open')`),
// and the CSS has no `.open` rule at all. Must flag via the var-binding path.
func TestVisibilityToggle_FlagsVarBoundReveal(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "b.css", `.hidden{display:none!important} .dropdown{position:absolute}`)
	saveKind(t, db, "page", "/",
		`<button id="btnMenu">Menu</button>
		 <div id="templates-menu" class="dropdown hidden">tpl</div>`)
	saveKind(t, db, "file", "saves.js",
		`const drawer = document.getElementById('templates-menu');
		 document.getElementById('btnMenu').addEventListener('click', () => drawer.classList.toggle('open'));`)

	issues := toggleIssues(t, db)
	if len(issues) != 1 || issues[0].Signature != "templates-menu" || issues[0].Category != "dead_toggle" {
		t.Fatalf("expected one dead_toggle for templates-menu, got: %+v", issues)
	}
}

// The clean case that must not flag: a button hidden by a
// responsive utility (`.mobile-only{display:none}` at base, revealed by a
// media-query rule), whose click handler toggles `open` on a DIFFERENT element
// (the drawer). The button's visibility is CSS-owned; must NOT flag.
func TestVisibilityToggle_CleanForMediaQueryReveal(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "board.css",
		`.mobile-only{display:none}
		 @media (max-width: 720px){
		   .mobile-only{display:inline-flex}
		   .panel{position:fixed;transform:translateX(100%)}
		   .panel.open{transform:none}
		 }`)
	saveKind(t, db, "page", "/",
		`<button id="panelToggle" class="btn mobile-only">Ledger</button>
		 <aside id="ledgerPanel" class="panel">entries</aside>`)
	saveKind(t, db, "file", "board.js",
		`document.getElementById('panelToggle').addEventListener('click',
		   () => document.getElementById('ledgerPanel').classList.toggle('open'));`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("a media-query-revealed control must not flag, got: %+v", issues)
	}
}

// The correct convention (a state class WITH a matching rule): #sidebar hidden,
// button toggles `open`, and `#sidebar.open{display:flex}` reveals it. Clean.
func TestVisibilityToggle_CleanWhenRevealRuleExists(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.hidden{display:none} #sidebar.open{display:flex}`)
	saveKind(t, db, "page", "/",
		`<button id="btn">P</button><div id="sidebar" class="panel hidden">x</div>`)
	saveKind(t, db, "file", "app.js",
		`document.getElementById('btn').addEventListener('click', () => document.getElementById('sidebar').classList.toggle('open'));`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("a matching #id.open reveal rule must be clean, got: %+v", issues)
	}
}

// The other correct convention: toggle the hiding class itself. Clean.
func TestVisibilityToggle_CleanWhenTogglesHideClass(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.hidden{display:none!important}`)
	saveKind(t, db, "page", "/",
		`<button id="btn">P</button><div id="sidebar" class="panel hidden">x</div>`)
	saveKind(t, db, "file", "app.js",
		`document.getElementById('btn').addEventListener('click', () => document.getElementById('sidebar').classList.toggle('hidden'));`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("toggling the hide class itself must be clean, got: %+v", issues)
	}
}

// An element hidden by a class but never touched by JS is intentionally hidden
// (a template, an off state), not a dead control. Must not flag.
func TestVisibilityToggle_IgnoresHiddenElementNoJS(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.hidden{display:none}`)
	saveKind(t, db, "page", "/", `<template id="rowTpl" class="hidden"><li></li></template>`)
	saveKind(t, db, "file", "app.js", `console.log('no toggles here');`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("a hidden element no JS reveals must not be flagged, got: %+v", issues)
	}
}

// Revealing by a direct style change (not a class) is a valid convention the
// class-reconciliation must not misread as inert. Clean.
func TestVisibilityToggle_CleanWhenStyleReveal(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.hidden{display:none}`)
	saveKind(t, db, "page", "/",
		`<button id="btn">P</button><div id="sidebar" class="panel hidden">x</div>`)
	saveKind(t, db, "file", "app.js",
		`const s = document.getElementById('sidebar');
		 document.getElementById('btn').addEventListener('click', () => { s.style.display = 'flex'; });`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("a direct style reveal must be clean, got: %+v", issues)
	}
}

// The dominant hide idiom in the corpus is the boolean `hidden` attribute, not a
// class. An element hidden by `hidden` whose only reveal toggles an inert class is
// just as dead - the class can't clear the attribute. Must flag.
func TestVisibilityToggle_FlagsHiddenAttrWithInertToggle(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.panel{padding:8px}`)
	saveKind(t, db, "page", "/",
		`<button id="btn">Open</button><div id="drawer" class="panel" hidden>x</div>`)
	saveKind(t, db, "file", "app.js",
		`const d = document.getElementById('drawer');
		 document.getElementById('btn').addEventListener('click', () => d.classList.toggle('open'));`)

	issues := toggleIssues(t, db)
	if len(issues) != 1 || issues[0].Signature != "drawer" || issues[0].Category != "dead_toggle" {
		t.Fatalf("expected one dead_toggle for #drawer (hidden attr), got: %+v", issues)
	}
	if !strings.Contains(issues[0].Message, "hidden` attribute") {
		t.Errorf("message should name the hidden attribute: %q", issues[0].Message)
	}
}

// The correct reveal for a `hidden`-attribute element clears the attribute
// (`el.hidden = false`). Even alongside an unrelated class toggle, that is clean.
func TestVisibilityToggle_CleanWhenHiddenAttrCleared(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.panel{padding:8px}`)
	saveKind(t, db, "page", "/",
		`<button id="btn">Open</button><div id="drawer" class="panel" hidden>x</div>`)
	saveKind(t, db, "file", "app.js",
		`const d = document.getElementById('drawer');
		 document.getElementById('btn').addEventListener('click', () => { d.hidden = false; d.classList.add('anim'); });`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("clearing the hidden attribute is a correct reveal, must be clean, got: %+v", issues)
	}
}

// removeAttribute('hidden') is the other correct reveal and must reconcile.
func TestVisibilityToggle_CleanWhenHiddenAttrRemoved(t *testing.T) {
	db := setupToggleDB(t)
	saveKind(t, db, "file", "app.css", `.panel{padding:8px}`)
	saveKind(t, db, "page", "/",
		`<button id="btn">Open</button><div id="drawer" class="panel" hidden>x</div>`)
	saveKind(t, db, "file", "app.js",
		`const d = document.getElementById('drawer');
		 document.getElementById('btn').addEventListener('click', () => { d.classList.add('open'); d.removeAttribute('hidden'); });`)

	if issues := toggleIssues(t, db); len(issues) != 0 {
		t.Fatalf("removeAttribute('hidden') is a correct reveal, must be clean, got: %+v", issues)
	}
}

// aria-hidden / data-hidden and a `hidden` token inside a class value must NOT be
// read as the boolean hidden attribute (no false candidate).
func TestVisibilityToggle_IgnoresAriaAndClassHiddenLookalikes(t *testing.T) {
	if hasHiddenAttr(` class="row hidden-lg" aria-hidden="true" data-hidden="1" `) {
		t.Error("aria-hidden/data-hidden/class token must not count as the boolean hidden attribute")
	}
	if !hasHiddenAttr(` id="x" hidden `) {
		t.Error("a bare hidden attribute must be detected")
	}
}
