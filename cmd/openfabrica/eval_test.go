package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/security"
)

func TestParseBriefFile(t *testing.T) {
	brief, amb := parseBriefFile("---\r\nambition: high\r\n---\r\nA tip calculator.\r\n", "medium")
	if brief != "A tip calculator." || amb != "high" {
		t.Errorf("front matter: %q %q", brief, amb)
	}
	brief, amb = parseBriefFile("A kanban board.\n", "medium")
	if brief != "A kanban board." || amb != "medium" {
		t.Errorf("plain: %q %q", brief, amb)
	}
}

// The minted token is an admin session the server's own guard accepts: signed with
// the instance key, for the admin user, at its current token version.
func TestEvalAdminToken(t *testing.T) {
	dir := t.TempDir()
	core, err := db.Open(filepath.Join(dir, "openfabrica.db"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := models.CreateUser(core.DB, "owner", "x", "admin", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	core.Close()
	secret := strings.Repeat("k", 32)
	tok, err := evalAdminToken(&config.Config{DBPath: filepath.Join(dir, "openfabrica.db"), JWTSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := security.NewJWTManager(secret, time.Hour).Validate(tok)
	if err != nil {
		t.Fatalf("token does not validate: %v", err)
	}
	if claims.UserID != u.ID || claims.Role != "admin" || claims.ProjectID != 0 {
		t.Errorf("claims = %+v", claims)
	}
}

// Collection reads the latest run: unfinished plan items, feature coverage, the final
// sweep (acceptance findings separated from the rest), the latest spec verdict per
// item, and the run's token totals.
func TestCollectResult(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.CloseAll)
	plan := `{"tasks":[{"title":"Board","covers":["f1"]}],"exclusions":["f3: later"],` +
		`"features":[{"id":"f1","name":"Board"},{"id":"f2","name":"Staff page"},{"id":"f3","name":"Loyalty"}]}`
	for _, q := range []string{
		`INSERT INTO of_runs (id, status, stage, plan_json, started_at, completed_at) VALUES ('run_1', 'completed', 'live', '` + plan + `', '2026-10-16 10:00:00', '2026-10-16 10:06:00')`,
		`INSERT INTO of_tasks (id, title, status, origin, position) VALUES ('board', 'Board', 'done', 'plan', 0), ('staff', 'Staff', 'pending', 'plan', 1), ('fix-1-x', 'Fix', 'done', 'repair', 2)`,
		`INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES ('run_1', 'validation', '', '{"issues":[{"severity":"error","category":"inert_click"},{"severity":"error","category":"webfont"},{"severity":"warning","category":"api_target"}]}')`,
		`INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES ('run_1', 'issue', 'spec_audit', '{"verdicts":[{"id":"board","verdict":"partial"},{"id":"staff","verdict":"missing"}]}')`,
		`INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES ('run_1', 'issue', 'spec_audit', '{"verdicts":[{"id":"board","verdict":"met"}]}')`,
		`INSERT INTO of_llm_calls (run_id, source, model, input_tokens, output_tokens, cache_read_input_tokens) VALUES ('run_1', 'engine', 'm', 100, 50, 900), ('run_1', 'engine', 'm', 10, 5, 90)`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	var r evalResult
	if err := collectResult(pdb.Reader(), &r); err != nil {
		t.Fatal(err)
	}
	if r.RunID != "run_1" || r.Status != "completed" || r.DurationSec != 360 {
		t.Errorf("run: %+v", r)
	}
	if r.Tasks != 2 || r.Unfinished != 1 || r.RepairsDone != 1 {
		t.Errorf("tasks: %d/%d unfinished, %d repairs", r.Unfinished, r.Tasks, r.RepairsDone)
	}
	if r.Features != 3 || r.FeaturesOut != 1 || r.FeaturesLost != 1 {
		t.Errorf("features: %d, excluded %d, uncovered %d", r.Features, r.FeaturesOut, r.FeaturesLost)
	}
	if r.Errors["inert_click"] != 1 || r.Errors["webfont"] != 1 || r.Warnings["api_target"] != 1 {
		t.Errorf("sweep: %v %v", r.Errors, r.Warnings)
	}
	if r.Acceptance != 2 { // the inert click plus the unfinished item; webfont is not one a user meets
		t.Errorf("acceptance = %d, want 2", r.Acceptance)
	}
	if r.SpecMet != 1 || r.SpecMissing != 1 || r.SpecPartial != 0 || r.SpecMetRatio != 0.5 {
		t.Errorf("spec: met %d partial %d missing %d ratio %v", r.SpecMet, r.SpecPartial, r.SpecMissing, r.SpecMetRatio)
	}
	if r.Calls != 2 || r.OutputTokens != 55 || r.BilledInput != 1100 {
		t.Errorf("cost: %d calls, out %d, billed %d", r.Calls, r.OutputTokens, r.BilledInput)
	}
}

func TestSummarizeAndCompare(t *testing.T) {
	a := summarize([]evalResult{
		{Brief: "01-tip", Status: "completed", Acceptance: 3, SpecMet: 2, SpecPartial: 2, OutputTokens: 10_000, BilledInput: 400_000},
		{Brief: "02-kanban", Status: "failed", Acceptance: 5, SpecMet: 1, SpecMissing: 1, Errors: map[string]int{"runtime": 2}},
	})
	if a.Totals.Completed != 1 || a.Totals.Acceptance != 8 || a.Totals.SpecAudited != 6 || a.Totals.Errors != 2 {
		t.Fatalf("totals: %+v", a.Totals)
	}
	b := summarize([]evalResult{
		{Brief: "01-tip", Status: "completed", Acceptance: 1, SpecMet: 4, OutputTokens: 12_000, BilledInput: 500_000},
		{Brief: "03-new", Status: "completed"},
	})
	out := renderCompare(a, b)
	if !strings.Contains(out, "01-tip") || strings.Contains(out, "02-kanban") || strings.Contains(out, "03-new") {
		t.Errorf("compare covers only the briefs both runs share:\n%s", out)
	}
	if !strings.Contains(out, "3 -> 1") || !strings.Contains(out, "50% -> 100%") || !strings.Contains(out, "1.25x") {
		t.Errorf("compare deltas:\n%s", out)
	}
	if !strings.Contains(renderSummary(a), "TOTAL") {
		t.Error("summary table has a total row")
	}
}
