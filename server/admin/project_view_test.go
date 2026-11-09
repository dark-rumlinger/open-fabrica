package admin

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	sdeps "openfabrica/server/deps"
)

// newPhaseTestRouter serves the project list and get routes over an in-memory
// global DB seeded with three projects: a published building one (with a run
// parked on an owner question), an unpublished live one, and a paused one.
func newPhaseTestRouter(t *testing.T) chi.Router {
	t.Helper()
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	if _, err := mem.Exec(projectsTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, row := range []struct {
		id        int
		published int
		mode      string
	}{
		{1, 1, "building"},
		{2, 0, "live"},
		{3, 1, "paused"},
	} {
		if _, err := mem.Exec(
			"INSERT INTO projects (id, name, published, mode, llm_model_id) VALUES (?, 'p', ?, ?, 1)",
			row.id, row.published, row.mode); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}

	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_runs (id, status, stage, pause_reason)
		VALUES ('r1', 'paused', 'plan', 'awaiting_owner_answers')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_questions (question, status, type)
		VALUES ('Brand colour?', 'pending', 'text')`); err != nil {
		t.Fatalf("seed question: %v", err)
	}

	deps := &Deps{Core: sdeps.Core{
		DB:               &db.DB{DB: mem},
		ProjectDBManager: mgr,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	h := &ProjectsHandler{deps: deps}
	r := chi.NewRouter()
	r.Get("/projects", h.List)
	r.Get("/projects/{projectID}", h.Get)
	r.Put("/projects/{projectID}/published", h.SetPublished)
	return r
}

func phaseOfJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	phase, ok := m["phase"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no phase object: %v", m)
	}
	return phase
}

// A single project reports the phase with run detail: a run parked on the owner
// is "needs_you" even though the stored mode is still building.
func TestProjectGetCarriesPhaseAndPublished(t *testing.T) {
	r := newPhaseTestRouter(t)

	got := getJSON(t, r, "/projects/1", http.StatusOK)
	phase := phaseOfJSON(t, got)
	if phase["key"] != "needs_you" || phase["pending"] != float64(1) {
		t.Errorf("phase = %v, want needs_you with one pending question", phase)
	}
	if got["mode"] != "building" {
		t.Errorf("mode = %v, want the stored building mode untouched", got["mode"])
	}
	if got["published"] != true {
		t.Errorf("published = %v, want true for an active project", got["published"])
	}

	live := getJSON(t, r, "/projects/2", http.StatusOK)
	if phaseOfJSON(t, live)["key"] != "live" || live["published"] != false {
		t.Errorf("live project = phase %v published %v, want live and unpublished (independent)", live["phase"], live["published"])
	}

	paused := getJSON(t, r, "/projects/3", http.StatusOK)
	pp := phaseOfJSON(t, paused)
	if pp["key"] != "paused" || paused["published"] != true {
		t.Errorf("paused project = phase %v published %v, want paused and still published", pp, paused["published"])
	}
	if pp["resume_to"] != "building" {
		t.Errorf("resume_to = %v, want building when no run ever completed", pp["resume_to"])
	}
}

// The list must not open every project's database, so it reports the phase from
// the mode alone unless a database is already open.
func TestProjectListCarriesPhaseForEveryProject(t *testing.T) {
	r := newPhaseTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d projects, want 3", len(items))
	}
	byID := map[float64]map[string]any{}
	for _, it := range items {
		byID[it["id"].(float64)] = it
		if _, ok := it["published"].(bool); !ok {
			t.Errorf("project %v has no published flag", it["id"])
		}
	}
	if phaseOfJSON(t, byID[1])["key"] != "needs_you" {
		t.Errorf("project 1 phase = %v, want needs_you from its already open database", byID[1]["phase"])
	}
	if phaseOfJSON(t, byID[2])["key"] != "live" || phaseOfJSON(t, byID[3])["key"] != "paused" {
		t.Errorf("phases = %v / %v, want live / paused", byID[2]["phase"], byID[3]["phase"])
	}
}

func putJSON(t *testing.T, r chi.Router, url, body string, wantCode int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != wantCode {
		t.Fatalf("PUT %s: got %d, want %d (body %s)", url, w.Code, wantCode, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// Publishing is its own switch: it flips visibility and leaves the phase alone.
func TestSetPublishedLeavesThePhaseAlone(t *testing.T) {
	r := newPhaseTestRouter(t)

	got := putJSON(t, r, "/projects/3/published", `{"published":false}`, http.StatusOK)
	if got["published"] != false {
		t.Errorf("published = %v, want false after unpublishing", got["published"])
	}
	if phaseOfJSON(t, got)["key"] != "paused" {
		t.Errorf("phase = %v, want the paused phase untouched by unpublishing", got["phase"])
	}

	got = putJSON(t, r, "/projects/2/published", `{"published":true}`, http.StatusOK)
	if got["published"] != true || phaseOfJSON(t, got)["key"] != "live" {
		t.Errorf("after publishing: published=%v phase=%v, want published and still live", got["published"], got["phase"])
	}

	if again := getJSON(t, r, "/projects/2", http.StatusOK); again["published"] != true {
		t.Errorf("published did not persist: %v", again["published"])
	}
	putJSON(t, r, "/projects/99/published", `{"published":true}`, http.StatusNotFound)
}
