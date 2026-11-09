package store

import (
	"database/sql"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

// setupPageAssetsDB builds the minimal schema UnlinkedPageArtifacts reads:
// of_files (bodies included, for import-graph parsing) and of_page_assets.
func setupPageAssetsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY,
			kind TEXT, path TEXT, body TEXT, scope TEXT,
			is_deleted INTEGER DEFAULT 0
		)`,
		`CREATE TABLE of_page_assets (
			page_path TEXT, file_path TEXT, position INTEGER DEFAULT 0
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func saveArt(t *testing.T, db *sql.DB, path, scope, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, scope, body) VALUES ('file', ?, ?, ?)`, path, scope, body); err != nil {
		t.Fatalf("insert %s: %v", path, err)
	}
}

func linkArt(t *testing.T, db *sql.DB, page, artifact string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_page_assets (page_path, file_path) VALUES (?, ?)`, page, artifact); err != nil {
		t.Fatalf("link %s: %v", artifact, err)
	}
}

// The core of the link-only-entry pattern: linking just the entry module and
// letting its relative imports pull the rest must NOT flag the imported files.
func TestUnlinkedPageArtifacts_EntryImportsAreReached(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `
		import { State } from './state.js';
		import { Layers } from './layers/model.js';
		import * as util from './util';
	`)
	saveArt(t, db, "js/state.js", "page", `export class State {}`)
	saveArt(t, db, "js/layers/model.js", "page", `export class Layers {}`)
	saveArt(t, db, "js/util.js", "page", `export const x = 1;`) // imported without extension
	linkArt(t, db, "/", "js/app.js")                            // ONLY the entry is linked

	if got := UnlinkedPageArtifacts(db); len(got) != 0 {
		t.Fatalf("entry imports must be reachable, got flagged: %v", got)
	}
}

// Reachability is transitive: a module imported by an imported module counts.
func TestUnlinkedPageArtifacts_TransitiveAndRelativeParents(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `import { reg } from './tools/registry.js';`)
	saveArt(t, db, "js/tools/registry.js", "page", `
		import { brush } from './brush.js';
		import { comp } from '../canvas/compositor.js';
		export const reg = {};
	`)
	saveArt(t, db, "js/tools/brush.js", "page", `export const brush = {};`)
	saveArt(t, db, "js/canvas/compositor.js", "page", `export const comp = {};`)
	linkArt(t, db, "/", "js/app.js")

	if got := UnlinkedPageArtifacts(db); len(got) != 0 {
		t.Fatalf("transitive + ../ imports must be reachable, got flagged: %v", got)
	}
}

// A page-JS file that no linked module imports - and that isn't linked - is
// still dead weight and must be flagged.
func TestUnlinkedPageArtifacts_TrulyOrphanStillFlagged(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `import { State } from './state.js';`)
	saveArt(t, db, "js/state.js", "page", `export class State {}`)
	saveArt(t, db, "js/orphan.js", "page", `export const dead = 1;`) // nobody imports it
	linkArt(t, db, "/", "js/app.js")

	got := UnlinkedPageArtifacts(db)
	want := []string{"js/orphan.js"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected only orphan flagged, got %v", got)
	}
}

// Two unlinked modules importing each other are NOT rescued - with no linked
// entry rooting the graph, nothing loads them.
func TestUnlinkedPageArtifacts_UnrootedCycleFlagged(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/a.js", "page", `import './b.js'; export const a = 1;`)
	saveArt(t, db, "js/b.js", "page", `import './a.js'; export const b = 1;`)
	// nothing linked

	got := UnlinkedPageArtifacts(db)
	want := []string{"js/a.js", "js/b.js"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unrooted modules must both be flagged, got %v", got)
	}
}

// CSS is never reachable through a JS import (browsers can't import a
// stylesheet), so an unlinked CSS file is flagged even if a linked module
// names it in an import.
func TestUnlinkedPageArtifacts_CSSMustBeLinked(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `import './editor.css'; export const x = 1;`)
	saveArt(t, db, "css/editor.css", "page", `body{}`)
	linkArt(t, db, "/", "js/app.js")

	got := UnlinkedPageArtifacts(db)
	want := []string{"css/editor.css"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CSS must be linked directly, got %v", got)
	}
}

// A representative regression: the entry loads its leaves via a dynamic
// `import(path)` loop, opaque to the static graph, so they surface as orphans yet
// load fine at runtime. The result must be Uncertain so the validator downgrades
// to a warning instead of a destructive hard error.
func TestUnlinkedPageArtifactsClassified_OpaqueDynamicImportMakesOrphansUncertain(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "space/main.js", "page", `
		import { state } from './state.js';
		const leaves = [
			['planet', './planet.js'],
			['multitool', './multitool.js'],
		];
		async function registerAll() {
			for (const [name, path] of leaves) {
				const mod = await import(path);
				registerSubsystem(name, mod.default);
			}
		}
	`)
	saveArt(t, db, "space/state.js", "page", `export const state = {};`)
	saveArt(t, db, "space/planet.js", "page", `export default {};`)
	saveArt(t, db, "space/multitool.js", "page", `export default {};`)
	linkArt(t, db, "/", "space/main.js") // ONLY the entry is linked

	res := UnlinkedPageArtifactsClassified(db)
	want := []string{"space/multitool.js", "space/planet.js"}
	if !reflect.DeepEqual(res.Orphans, want) {
		t.Fatalf("expected the dynamically-loaded leaves flagged, got %v", res.Orphans)
	}
	if !res.Uncertain {
		t.Fatal("an opaque dynamic import in the entry must make the orphan set Uncertain")
	}
}

// A LITERAL dynamic import is already a graph edge: import('./planet.js') reaches
// planet.js, so it is neither an orphan nor a source of uncertainty.
func TestUnlinkedPageArtifactsClassified_LiteralDynamicImportIsReached(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "space/main.js", "page", `
		import { state } from './state.js';
		const p = await import('./planet.js');
	`)
	saveArt(t, db, "space/state.js", "page", `export const state = {};`)
	saveArt(t, db, "space/planet.js", "page", `export default {};`)
	linkArt(t, db, "/", "space/main.js")

	res := UnlinkedPageArtifactsClassified(db)
	if len(res.Orphans) != 0 {
		t.Fatalf("a literal dynamic import must be reachable, got flagged: %v", res.Orphans)
	}
	if res.Uncertain {
		t.Fatal("a literal dynamic import must not make anything uncertain")
	}
}

// A genuinely dead module in a project with NO dynamic imports stays certain - the
// hard error is correct there and must not be weakened.
func TestUnlinkedPageArtifactsClassified_StaticOnlyOrphanIsCertain(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `import { State } from './state.js';`)
	saveArt(t, db, "js/state.js", "page", `export class State {}`)
	saveArt(t, db, "js/orphan.js", "page", `export const dead = 1;`)
	linkArt(t, db, "/", "js/app.js")

	res := UnlinkedPageArtifactsClassified(db)
	want := []string{"js/orphan.js"}
	if !reflect.DeepEqual(res.Orphans, want) {
		t.Fatalf("expected only orphan flagged, got %v", res.Orphans)
	}
	if res.Uncertain {
		t.Fatal("a static-only project must keep its orphan finding certain")
	}
}

// A commented-out dynamic import must not trigger a false downgrade: the orphan
// stays certain.
func TestUnlinkedPageArtifactsClassified_CommentedOpaqueImportIgnored(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `
		import { State } from './state.js';
		// const mod = await import(path); // legacy loader, removed
	`)
	saveArt(t, db, "js/state.js", "page", `export class State {}`)
	saveArt(t, db, "js/orphan.js", "page", `export const dead = 1;`)
	linkArt(t, db, "/", "js/app.js")

	res := UnlinkedPageArtifactsClassified(db)
	if res.Uncertain {
		t.Fatal("a commented-out import() must not make the orphan uncertain")
	}
}

// A stray kind=file copy of the user-flow script is test data, not a page module:
// flagging it as unlinked sent a fix-pass to attach of_test.js to the page.
func TestUnlinkedPageArtifacts_FlowScriptNeverFlagged(t *testing.T) {
	db := setupPageAssetsDB(t)
	saveArt(t, db, "js/app.js", "page", `export const a = 1;`)
	saveArt(t, db, FlowScriptPath, "page", `export default { flows: [] };`)
	linkArt(t, db, "/", "js/app.js")
	if got := UnlinkedPageArtifacts(db); len(got) != 0 {
		t.Fatalf("the flow script must never be reported as unlinked, got %v", got)
	}
}
