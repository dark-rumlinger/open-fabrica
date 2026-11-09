package validators

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

// setupPWADB builds of_files + of_runs and seeds the latest run's plan_json.
func setupPWADB(t *testing.T, plan Plan) *sql.DB {
	t.Helper()
	db := setupPageDB(t)
	if _, err := db.Exec(`CREATE TABLE of_runs (
		id INTEGER PRIMARY KEY,
		plan_json TEXT,
		started_at TEXT DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("create of_runs: %v", err)
	}
	pj, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_runs (plan_json) VALUES (?)`, string(pj)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return db
}

// pwaPlan: the canonical installable-PWA plan shape - a final task scoped to the
// manage_project tool (the enable_pwa carrier), owning no paths.
func pwaPlan() Plan {
	return Plan{
		Goal: "A habit tracker",
		Tasks: []PlanTaskSpec{
			{ID: "today-page", Title: "Today page", Paths: []string{"/"}},
			{ID: "enable-pwa", Title: "Enable PWA", Tools: []string{"manage_project"}},
		},
	}
}

// A representative failure: the plan calls for an installable PWA but
// enable_pwa never ran, so no manifest / service worker / layout link exists.
func TestPWAReadiness_FlagsMissingPWA(t *testing.T) {
	db := setupPWADB(t, pwaPlan())
	saveKind(t, db, "page", "/", `<section>Today</section>`)
	saveKind(t, db, "layout", "default", `<head></head>`)

	issues := pwaReadinessValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 pwa_not_enabled issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].Category != "pwa_not_enabled" || issues[0].Severity != "error" || !issues[0].Deterministic {
		t.Errorf("unexpected issue: %+v", issues[0])
	}
}

// Once enable_pwa has run (manifest.json + service-worker.js as files, layout
// patched with the manifest link), the validator is clean.
func TestPWAReadiness_CleanWhenEnabled(t *testing.T) {
	db := setupPWADB(t, pwaPlan())
	saveKind(t, db, "page", "/", `<section>Today</section>`)
	saveKind(t, db, "file", "manifest.json", `{"name":"x"}`)
	saveKind(t, db, "file", "service-worker.js", `self.addEventListener('install',()=>{})`)
	saveKind(t, db, "layout", "default", `<head><link rel="manifest" href="/manifest.json"></head>`)

	if issues := (pwaReadinessValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("enabled PWA must be clean, got: %+v", issues)
	}
}

// A project that was never meant to be a PWA must not be flagged.
func TestPWAReadiness_SkipsNonPWA(t *testing.T) {
	db := setupPWADB(t, Plan{
		Goal:  "A plain marketing site",
		Tasks: []PlanTaskSpec{{ID: "home", Title: "Home", Paths: []string{"/"}}},
	})
	saveKind(t, db, "page", "/", `<section>Home</section>`)

	if issues := (pwaReadinessValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("non-PWA project must not be flagged, got: %+v", issues)
	}
}

func TestPlanRequiresPWA(t *testing.T) {
	// Required only via an explicit manage_project (enable_pwa) task.
	if db := setupPWADB(t, pwaPlan()); !planRequiresPWA(db) {
		t.Error("manage_project task should mark PWA required")
	}
	// Prose alone must NOT trigger: a game/tool whose copy mentions "offline" or
	// "installable" but has no enable_pwa task is not a PWA (the over-reach fix).
	proseDB := setupPWADB(t, Plan{Goal: "An installable offline game you add to your home screen", Tasks: []PlanTaskSpec{{Title: "Home"}}})
	if planRequiresPWA(proseDB) {
		t.Error("prose mentioning offline/installable must NOT mark PWA required without an enable_pwa task")
	}
	// A plain site is not a PWA.
	plainDB := setupPWADB(t, Plan{Goal: "A blog", Tasks: []PlanTaskSpec{{Title: "Home"}}})
	if planRequiresPWA(plainDB) {
		t.Error("plain site should not be marked PWA required")
	}
}

// A layout-less single-page project (e.g. a canvas game) that legitimately
// requested a PWA must NOT be flagged once manifest.json + service-worker.js
// exist: there is no `default` layout for enable_pwa to patch the <link> into, so
// requiring it would raise an issue no fix-pass could ever resolve.
func TestPWAReadiness_LayoutlessSkipsLinkCheck(t *testing.T) {
	db := setupPWADB(t, pwaPlan())
	saveKind(t, db, "page", "/", `<canvas id="game"></canvas>`)
	saveKind(t, db, "file", "manifest.json", `{"name":"x"}`)
	saveKind(t, db, "file", "service-worker.js", `self.addEventListener('install',()=>{})`)
	// No layout row at all.

	if issues := (pwaReadinessValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("layout-less PWA with manifest+SW must not be flagged, got: %+v", issues)
	}
}
