package validators

// A bare reference to an ESM library's conventional global (`THREE.Color` with
// three declared as kind="module") must drive a fix-pass, because no such
// global exists. Nothing else catches it: the call-position check is
// structurally blind (`new THREE.Color()` calls THREE.Color, not THREE), the
// page probe never clicks, and at warning severity fixableErrors excludes the
// member-position finding. Severity therefore turns on whether a declared
// library could be supplying the name as an ambient global. These tests pin
// both directions of that call.

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"openfabrica/analysis"
	"openfabrica/engine"
	"openfabrica/store"
)

// planTestDB builds the minimal of_runs row unboundRefSeverity reads.
func planTestDB(t *testing.T, planJSON string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_runs (id INTEGER PRIMARY KEY, plan_json TEXT, started_at TEXT)`); err != nil {
		t.Fatalf("create of_runs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_runs (plan_json, started_at) VALUES (?, '2026-07-16')`, planJSON); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return db
}

func TestLibraryDefinesGlobal(t *testing.T) {
	cases := []struct {
		name string
		lib  LibrarySpec
		want bool
	}{
		// An importmap ESM is reachable only by `import` and defines no global, so a
		// bare `THREE` in such a project cannot resolve.
		{"esm by kind", LibrarySpec{Name: "three.js", Kind: "module", ModuleName: "three",
			URL: "https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js"}, false},
		// A font stylesheet renders as <link> - no JS binding at all. It must not veto
		// the promotion, or a project that merely uses a webfont stays unguarded.
		{"font stylesheet", LibrarySpec{Name: "quicksand-font",
			URL: "https://fonts.googleapis.com/css2?family=Quicksand&display=swap"}, false},
		// The renderer emits type="module" for an ESM url even when kind is unset.
		{"esm by url, kind unset", LibrarySpec{Name: "x",
			URL: "https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js"}, false},

		// The real reason the check had to stay a warning: this DOES define a global,
		// and the platform never learns it is called `Chart`.
		{"classic umd defines a global", LibrarySpec{Name: "chart.js",
			URL: "https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.js"}, true},
		{"kind classic is explicit", LibrarySpec{Name: "gsap", Kind: "classic",
			URL: "https://cdn.jsdelivr.net/npm/gsap@3/dist/gsap.min.js"}, true},
		// Unknown shape -> assume it could. Conservative beats a false fix-pass.
		{"no url", LibrarySpec{Name: "mystery"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirrors the renderer's injection decision: only a library that lands in
			// its classic `<script src>` branch can define an ambient global.
			if got := analysis.LibraryDefinesGlobal(tc.lib.ExternalLib()); got != tc.want {
				t.Errorf("LibraryDefinesGlobal(%+v) = %v, want %v", tc.lib, got, tc.want)
			}
		})
	}
}

// End to end: a name reached through a member access, no import, and only module/font
// libraries declared. It must be a fix-pass-driving error.
func TestUnboundRefIsFixableWhenNoLibraryCanDefineIt(t *testing.T) {
	body := `function grassColor(h) { return new THREE.Color(0.2, h, 0.1); }
export function buildTerrain() { return grassColor(1); }`

	// The call-position scan cannot see it - this is why the member check is the only
	// thing standing between THREE and production.
	if refs := analysis.UndefinedCallRefs("terrain.js", body); len(refs) != 0 {
		t.Fatalf("premise changed: the call-position scan now sees %v; this test guards the MEMBER path", refs)
	}
	var member bool
	for _, r := range analysis.UndefinedRefs("terrain.js", body) {
		if r.Name == "THREE" && r.Kind == analysis.RefMember {
			member = true
		}
	}
	if !member {
		t.Fatal("the member-position scan no longer reports THREE - this defect class would be invisible")
	}

	pdb := planTestDB(t, `{"external_libraries":[
		{"name":"three.js","kind":"module","module_name":"three","url":"https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js"},
		{"name":"quicksand-font","url":"https://fonts.googleapis.com/css2?family=Quicksand&display=swap"}
	]}`)
	sev, det, _ := unboundRefSeverity(pdb)
	if sev != store.SeverityError || !det {
		t.Errorf("got severity=%q deterministic=%v, want error/true.\n"+
			"No declared library can define a global here (an importmap ESM and a font stylesheet define none), "+
			"so an unbound name is genuinely undefined. As a warning it drives no fix-pass, which is how "+
			"the name reaches production despite the validator naming the file.", sev, det)
	}
	fix := engine.FixableErrors([]Issue{{Category: "undefined_reference", File: "terrain.js", Severity: sev, Deterministic: det}})
	if len(fix) != 1 {
		t.Error("the finding still does not reach fixableErrors, so it drives no fix-pass - the whole point of the promotion")
	}
}

// The other direction, which is why this was warn-first: with a UMD library declared
// and NO global_name on it, an unbound name might be its global and the platform still
// cannot know. Naming the global is the escape hatch - see the test below.
func TestUnboundRefStaysWarningWhenAUMDLibraryIsDeclared(t *testing.T) {
	pdb := planTestDB(t, `{"external_libraries":[
		{"name":"chart.js","url":"https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.js"}
	]}`)
	sev, det, _ := unboundRefSeverity(pdb)
	if sev != store.SeverityWarning || det {
		t.Errorf("got severity=%q deterministic=%v, want warning/false. chart.umd.js renders as a classic "+
			"<script src> and defines a global the platform never learns the name of, so `Chart` would "+
			"false-positive into a fix-pass against working code.", sev, det)
	}
}

// An unreadable/absent plan must degrade to a warning, never to a false error.
func TestUnboundRefStaysWarningWithoutAPlan(t *testing.T) {
	if sev, det, _ := unboundRefSeverity(nil); sev != store.SeverityWarning || det {
		t.Errorf("a nil DB must degrade to warning, got %q/%v", sev, det)
	}
	pdb := planTestDB(t, `{not json`)
	if sev, det, _ := unboundRefSeverity(pdb); sev != store.SeverityWarning || det {
		t.Errorf("an unparseable plan must degrade to warning, got %q/%v", sev, det)
	}
}

// The escape hatch: a classic UMD library that NAMES its global. The name is
// allowlisted (so `Chart` is not a finding) and every OTHER unbound name becomes a
// blocking error - the whole point of adding global_name to LibrarySpec. Without
// this, one webfont-plus-UMD project disarmed the check for every module in it.
func TestUnboundRefIsFixableWhenEveryClassicLibraryNamesItsGlobal(t *testing.T) {
	pdb := planTestDB(t, `{"external_libraries":[
		{"name":"chart.js","global_name":"Chart","url":"https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.js"},
		{"name":"quicksand-font","url":"https://fonts.googleapis.com/css2?family=Quicksand&display=swap"}
	]}`)
	sev, det, globals := unboundRefSeverity(pdb)
	if sev != store.SeverityError || !det {
		t.Errorf("got severity=%q deterministic=%v, want error/true: every globals-capable library named "+
			"its global, so nothing undeclared can be supplying an unbound name.", sev, det)
	}
	if !globals["Chart"] {
		t.Error("the declared global is not allowlisted, so `Chart.register(...)` would false-positive")
	}
	if globals["THREE"] {
		t.Error("an undeclared name must NOT be allowlisted - that is what keeps the check useful")
	}
}

// One unnamed classic library is enough to disarm the whole project: the platform
// cannot tell that library's global from a typo, so blocking would fix-pass against
// working code. The named sibling is still allowlisted, so it never false-positives.
func TestOneUnnamedClassicLibraryKeepsTheProjectWarning(t *testing.T) {
	pdb := planTestDB(t, `{"external_libraries":[
		{"name":"chart.js","global_name":"Chart","url":"https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.js"},
		{"name":"mystery","url":"https://cdn.jsdelivr.net/npm/mystery@1/dist/mystery.min.js"}
	]}`)
	sev, det, globals := unboundRefSeverity(pdb)
	if sev != store.SeverityWarning || det {
		t.Errorf("got severity=%q deterministic=%v, want warning/false: `mystery` names no global, so an "+
			"unbound name might be it.", sev, det)
	}
	if !globals["Chart"] {
		t.Error("a named global stays allowlisted even when a sibling is unnamed")
	}
}

// A project declaring a classic qrcode script.
// With global_name set, `window.qrcode` is a live read; without it, the dead-read
// finding could only ever be a warning, so a permanently dead feature could ship
// with known_issues=false.
func TestDeclaredClassicGlobalIsTreatedAsProduced(t *testing.T) {
	globals, exact := analysis.LibraryGlobals([]analysis.ExternalLib{
		{Name: "qrcode", URL: "https://cdn.jsdelivr.net/npm/qrcode-generator@1.4.4/qrcode.js", GlobalName: "qrcode"},
	})
	if !exact {
		t.Error("a classic library that names its global must make the answer exact")
	}
	if !globals["qrcode"] {
		t.Error("`window.qrcode` must count as produced by the declared library")
	}
}
