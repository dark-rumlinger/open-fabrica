package public

import (
	"database/sql"
	"strings"
	"testing"
)

// setupRenderLibDB builds an in-memory project DB with of_runs (carrying the
// current plan) and of_files, the tables the renderer's library loaders read.
func setupRenderLibDB(t *testing.T, planJSON string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE of_runs (
			id TEXT PRIMARY KEY, plan_json TEXT,
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY, kind TEXT, path TEXT, body TEXT,
			scope TEXT, metadata TEXT DEFAULT '{}', is_deleted INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`, planJSON); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return db
}

// The renderer's external-library injection must read plan.external_libraries
// from the current of_runs row. This guards the CRITICAL render path end-to-end: a project that
// declares CDN libraries gets them injected at request time - a classic lib as a
// <script src>, a module-kind lib via the importmap.
func TestLoadExternalLibsFromRun(t *testing.T) {
	plan := `{"external_libraries":[
		{"name":"marked","url":"https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js"},
		{"name":"three","url":"https://unpkg.com/three@0.160.0/build/three.module.js","kind":"module","module_name":"three"}
	]}`
	db := setupRenderLibDB(t, plan)

	tags := loadExternalLibTags(db, "")
	if !strings.Contains(tags, `<script src="https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js"`) {
		t.Errorf("classic library not injected from of_runs plan: %q", tags)
	}
	if strings.Contains(tags, "three.module.js") {
		t.Errorf("module-kind library must be wired via importmap, not a <script>: %q", tags)
	}

	imap := loadImportMap(db, nil)
	if !strings.Contains(imap, `"three"`) ||
		!strings.Contains(imap, "https://unpkg.com/three@0.160.0/build/three.module.js") {
		t.Errorf("module library not wired into importmap from of_runs plan: %q", imap)
	}
}

// With no run carrying a plan, the loaders fall open (inject nothing) instead of
// erroring on the missing plan.
func TestLoadExternalLibsNoPlan(t *testing.T) {
	db := setupRenderLibDB(t, "") // empty plan_json is not selected by the current-plan query
	if tags := loadExternalLibTags(db, ""); tags != "" {
		t.Errorf("no plan should inject nothing, got: %q", tags)
	}
	if imap := loadImportMap(db, nil); imap != "" {
		t.Errorf("no plan and no internal modules should yield no importmap, got: %q", imap)
	}
}

func TestBuildExternalLibTags_CSSFont(t *testing.T) {
	libs := []externalLibSpec{{
		URL: "https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap",
	}}
	out := buildExternalLibTags(libs, "")
	if !strings.Contains(out, `<link rel="stylesheet"`) {
		t.Errorf("font URL should emit a <link>, got: %s", out)
	}
	if strings.Contains(out, "<script") {
		t.Errorf("font URL must not emit a <script>, got: %s", out)
	}
	if !strings.Contains(out, `crossorigin="anonymous"`) {
		t.Errorf("cross-origin font link should carry crossorigin, got: %s", out)
	}
}

func TestBuildExternalLibTags_FontFile(t *testing.T) {
	// A raw gstatic .woff2 URL that slipped into external_libraries must preload,
	// never <script>: the browser refuses to execute a font as a script under
	// strict MIME checking ("MIME type 'font/woff2' is not executable").
	libs := []externalLibSpec{{
		URL: "https://fonts.gstatic.com/s/pressstart2p/v15/e3t4euO8T-267oIAQAu6jDQyK3nVivM.woff2",
	}}
	out := buildExternalLibTags(libs, "")
	if strings.Contains(out, "<script") {
		t.Errorf("font file must not emit a <script>, got: %s", out)
	}
	if !strings.Contains(out, `rel="preload"`) || !strings.Contains(out, `as="font"`) {
		t.Errorf("font file should preload as a font, got: %s", out)
	}
	if !strings.Contains(out, `type="font/woff2"`) {
		t.Errorf("woff2 preload should carry type=font/woff2, got: %s", out)
	}
	if !strings.Contains(out, `crossorigin="anonymous"`) {
		t.Errorf("cross-origin font preload needs crossorigin, got: %s", out)
	}
}

func TestBuildExternalLibTags_ClassicJS(t *testing.T) {
	libs := []externalLibSpec{{
		URL: "https://unpkg.com/three@0.160.0/build/three.min.js",
	}}
	out := buildExternalLibTags(libs, "")
	if !strings.Contains(out, `<script src="https://unpkg.com/three`) {
		t.Errorf("classic JS should emit a <script src>, got: %s", out)
	}
	if strings.Contains(out, `type="module"`) {
		t.Errorf("classic JS must not be type=module, got: %s", out)
	}
}

func TestBuildExternalLibTags_SkipsModuleKind(t *testing.T) {
	libs := []externalLibSpec{{
		URL:        "https://unpkg.com/three@0.160.0/build/three.module.js",
		Kind:       "module",
		ModuleName: "three",
	}}
	if out := buildExternalLibTags(libs, ""); out != "" {
		t.Errorf("module-kind libs are wired via importmap, expected no tag, got: %s", out)
	}
}

func TestBuildExternalLibTags_ESMDefenseInDepth(t *testing.T) {
	// kind unset but URL is ESM: emit type="module" so it still loads.
	libs := []externalLibSpec{{
		URL: "https://cdn.jsdelivr.net/npm/framer-motion@11.15.0/dist/es/index.mjs",
	}}
	out := buildExternalLibTags(libs, "")
	if !strings.Contains(out, `<script type="module" src="https://cdn.jsdelivr.net/npm/framer-motion`) {
		t.Errorf("classic-declared ESM URL should emit type=module, got: %s", out)
	}
}

func TestBuildExternalLibTags_Integrity(t *testing.T) {
	libs := []externalLibSpec{{
		URL:       "https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js",
		Integrity: "sha384-abcdef",
	}}
	out := buildExternalLibTags(libs, "")
	if !strings.Contains(out, `integrity="sha384-abcdef"`) {
		t.Errorf("integrity from the lib entry should be emitted, got: %s", out)
	}
	if !strings.Contains(out, `crossorigin="anonymous"`) {
		t.Errorf("integrity needs crossorigin to take effect, got: %s", out)
	}
}

func TestBuildExternalLibTags_FontIntegritySuppressed(t *testing.T) {
	// A stale/bogus hash persisted on a font endpoint must NOT be emitted: the
	// CSS is content-negotiated, so any build-time hash blocks the stylesheet in
	// the browser. crossorigin stays (harmless), integrity is dropped.
	libs := []externalLibSpec{{
		URL:       "https://fonts.googleapis.com/css2?family=Orbitron:wght@400;700&display=swap",
		Integrity: "sha384-stalehashthatwillnevermatch",
	}}
	out := buildExternalLibTags(libs, "")
	if !strings.Contains(out, `<link rel="stylesheet"`) {
		t.Errorf("font URL should emit a <link>, got: %s", out)
	}
	if strings.Contains(out, "integrity=") {
		t.Errorf("font endpoint must not carry integrity=, got: %s", out)
	}
	if !strings.Contains(out, `crossorigin="anonymous"`) {
		t.Errorf("font link should still carry crossorigin, got: %s", out)
	}
}

func TestBuildExternalLibTags_StableCSSKeepsIntegrity(t *testing.T) {
	// A versioned .css on an ordinary CDN is NOT content-negotiated; SRI stays.
	libs := []externalLibSpec{{
		URL:       "https://cdn.jsdelivr.net/npm/some-lib@1.2.3/dist/theme.css",
		Integrity: "sha384-realhash",
	}}
	out := buildExternalLibTags(libs, "")
	if !strings.Contains(out, `integrity="sha384-realhash"`) {
		t.Errorf("stable CSS should keep its integrity hash, got: %s", out)
	}
}

func TestBuildExternalLibTags_DedupeByURL(t *testing.T) {
	url := "https://fonts.googleapis.com/css2?family=Inter:wght@400&display=swap"
	libs := []externalLibSpec{{URL: url}}
	present := `<head><link rel="stylesheet" href="` + url + `"></head>`
	if out := buildExternalLibTags(libs, present); out != "" {
		t.Errorf("URL already in markup should not be re-injected, got: %s", out)
	}
}

func TestBuildExternalLibTags_DedupeByFilename(t *testing.T) {
	// Same file served from a different CDN host should still dedupe.
	libs := []externalLibSpec{{URL: "https://unpkg.com/three@0.160.0/build/three.min.js"}}
	present := `<script src="https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.min.js"></script>`
	if out := buildExternalLibTags(libs, present); out != "" {
		t.Errorf("same filename from another CDN should dedupe, got: %s", out)
	}
}

func TestBuildExternalLibTags_EmptyAndBlankURL(t *testing.T) {
	if out := buildExternalLibTags(nil, ""); out != "" {
		t.Errorf("no libs should emit nothing, got: %s", out)
	}
	if out := buildExternalLibTags([]externalLibSpec{{URL: "   "}}, ""); out != "" {
		t.Errorf("blank URL should emit nothing, got: %s", out)
	}
}
