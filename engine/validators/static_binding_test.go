package validators

import (
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// A cross-module const reassignment: a single-file parse reports it clean, so only
// the bundler catches it. The static validator must flag it as a deterministic
// js_binding error so a fix-pass repairs it before the project goes live, instead
// of relying on the timing-coupled runtime probe.
func TestStaticCode_FlagsConstReassignAsBinding(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/state.js", `export const R = 10;`)
	saveCodeFile(t, db, "files/planet.js", `import { R } from './state.js';
function make(r){ return { r: r }; }
function flat(g){ return g; }
export function build() {
  const g = make(R);
  g = flat(g);
  return g;
}`)

	issues := staticIssues(t, db)
	if !hasIssue(issues, "js_binding", "files/planet.js") {
		t.Fatalf("const reassignment must be flagged as js_binding, got %+v", issues)
	}
	for _, is := range issues {
		if is.Category == "js_binding" && is.File == "files/planet.js" {
			if !strings.Contains(is.Message, "constant") {
				t.Errorf("js_binding message should name the binding fault, got: %s", is.Message)
			}
			if !is.Deterministic {
				t.Errorf("js_binding must be deterministic so it can drive a fix-pass")
			}
		}
	}
}

// A clean module (let, not const) must not raise a js_binding false positive.
func TestStaticCode_CleanModuleNoBinding(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/state.js", `export const R = 10;`)
	saveCodeFile(t, db, "files/planet.js", `import { R } from './state.js';
function make(r){ return { r: r }; }
function flat(g){ return g; }
export function build() {
  let g = make(R);
  g = flat(g);
  return g;
}`)

	issues := staticIssues(t, db)
	if anyIssueForFile(issues, "files/planet.js") {
		t.Fatalf("a clean module must raise no static issue, got %+v", issues)
	}
}

// A wrong-sibling import: main.js imports a name from a sibling that does not
// export it. Even if a transient state let it slip past the save gate, the
// build-end static validator - which sees every file present and clean - must
// flag it as a module_imports error. This pins the out-of-loop backstop.
func TestStaticCode_FlagsWrongSiblingImportAtBuildEnd(t *testing.T) {
	db := setupStaticDB(t)
	// state.js exports isMuted; audio.js imports it but never re-exports it.
	saveCodeFile(t, db, "files/state.js", `export function isMuted(){ return false; }`)
	saveCodeFile(t, db, "files/audio.js", `import { isMuted } from './state.js';
export function initAudio(){ return isMuted(); }`)
	// main.js wrongly imports isMuted from audio.js.
	saveCodeFile(t, db, "files/main.js", `import { initAudio, isMuted } from './audio.js';
export function boot(){ initAudio(); return isMuted(); }`)

	issues := staticIssues(t, db)
	if !hasIssue(issues, "module_imports", "files/main.js") {
		t.Fatalf("importing a name the sibling never exports must be flagged at build end, got %+v", issues)
	}
}
