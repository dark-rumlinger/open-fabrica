package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// seedFontProject builds a UI project whose global CSS names families via the
// token style a design system uses, optionally declaring plan libraries.
func seedFontProject(t *testing.T, css, planJSON string) *sql.DB {
	t.Helper()
	db := setupToggleDB(t)
	saveKind(t, db, "page", "/", `<main><h1>Title</h1></main>`)
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, scope) VALUES ('file','site.css',?, 'global')`, css); err != nil {
		t.Fatalf("insert css: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE of_runs (id TEXT PRIMARY KEY, plan_json TEXT, started_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create of_runs: %v", err)
	}
	if planJSON != "" {
		if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`, planJSON); err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}
	return db
}

// The silent-fallback shape: the design direction names real faces, the token layer
// uses them, and nothing anywhere loads them. Must flag and name both families.
func TestWebfont_FlagsSilentFallback(t *testing.T) {
	css := `:root{--font-display:"Space Grotesk",system-ui,sans-serif;--font-mono:"JetBrains Mono",Consolas,monospace}
	body{font-family:var(--font-display)}
	code{font-family:var(--font-mono)}`
	db := seedFontProject(t, css, `{"external_libraries":[{"name":"three.js","url":"https://cdn.jsdelivr.net/npm/three@0.185.1/build/three.module.js"}]}`)

	issues := (webfontValidator{}).Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 webfont issue, got %d: %+v", len(issues), issues)
	}
	if !issues[0].Deterministic || issues[0].Category != "webfont" {
		t.Fatalf("must be a deterministic webfont finding, got %+v", issues[0])
	}
	for _, want := range []string{"Space Grotesk", "JetBrains Mono"} {
		if !strings.Contains(issues[0].Message, want) {
			t.Fatalf("message must name %q, got: %s", want, issues[0].Message)
		}
	}
}

// A declared Google Fonts stylesheet serves the family, so naming it is correct.
func TestWebfont_SilentWhenDeclared(t *testing.T) {
	css := `:root{--font-display:"Space Grotesk",system-ui,sans-serif}
	body{font-family:var(--font-display)}`
	db := seedFontProject(t, css, `{"external_libraries":[{"name":"Space Grotesk","url":"https://fonts.googleapis.com/css2?family=Space+Grotesk&display=swap"}]}`)

	if issues := (webfontValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("a declared font must not flag, got: %+v", issues)
	}
}

// An explicit system stack loads nothing by design and must never flag.
func TestWebfont_SilentOnSystemStack(t *testing.T) {
	css := `body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,Arial,sans-serif}
	code{font-family:Consolas,"Courier New",monospace}`
	db := seedFontProject(t, css, `{"external_libraries":[]}`)

	if issues := (webfontValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("a system stack must not flag, got: %+v", issues)
	}
}

// An @font-face rule ships a face this check cannot attribute per family, so it
// stays quiet rather than guessing.
func TestWebfont_SilentOnFontFace(t *testing.T) {
	css := `@font-face{font-family:"Custom Sans";src:url(/files/custom.woff2) format("woff2")}
	body{font-family:"Custom Sans",sans-serif}`
	db := seedFontProject(t, css, `{"external_libraries":[]}`)

	if issues := (webfontValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("@font-face must silence the check, got: %+v", issues)
	}
}

// A headless project has no page, so there is no typography to judge.
func TestWebfont_SkipsHeadless(t *testing.T) {
	db := setupToggleDB(t)
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, scope) VALUES ('file','api.css',?, 'global')`,
		`:root{--font-display:"Space Grotesk",sans-serif}body{font-family:var(--font-display)}`); err != nil {
		t.Fatalf("insert css: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE of_runs (id TEXT PRIMARY KEY, plan_json TEXT, started_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create of_runs: %v", err)
	}
	if issues := (webfontValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("headless must self-skip, got: %+v", issues)
	}
}
