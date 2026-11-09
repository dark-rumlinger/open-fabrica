package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db/models"
)

// The user-flows setting is chosen at creation, changed in settings and carried into a
// copy; an absent field leaves the default (off) or the stored choice alone.
func TestProjectUserFlowsSettingThroughTheAdminAPI(t *testing.T) {
	deps := newTransferTestDeps(t, t.TempDir())
	h := &ProjectsHandler{deps: deps}
	r := chi.NewRouter()
	r.Post("/projects", h.Create)
	r.Put("/projects/{projectID}", h.Update)
	r.Post("/projects/{projectID}/copy", h.Copy)

	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	enabled := func(id int) bool {
		t.Helper()
		p, err := models.GetProjectByID(deps.DB.DB, id)
		if err != nil {
			t.Fatalf("load project %d: %v", id, err)
		}
		return models.ProjectUserFlowsEnabled(p)
	}
	idOf := func(rec *httptest.ResponseRecorder) int {
		t.Helper()
		if rec.Code != http.StatusCreated {
			t.Fatalf("create/copy answered %d: %s", rec.Code, rec.Body.String())
		}
		var p struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p.ID
	}

	// Default: no field means off (the feature is experimental).
	dflt := idOf(send(http.MethodPost, "/projects", `{"name":"dflt","domain":"dflt.localhost","llm_model_id":1}`))
	if enabled(dflt) {
		t.Fatal("a project created without the field must have user-flow tests off")
	}

	// Chosen on at creation.
	on := idOf(send(http.MethodPost, "/projects", `{"name":"on","domain":"on.localhost","llm_model_id":1,"user_flows":true}`))
	if !enabled(on) {
		t.Fatal("a project created with user_flows true must run user-flow tests")
	}

	// Chosen off at creation.
	off := idOf(send(http.MethodPost, "/projects", `{"name":"off","domain":"off.localhost","llm_model_id":1,"user_flows":false}`))
	if enabled(off) {
		t.Fatal("a project created with user_flows false must have them off")
	}

	// A copy keeps the source's choice.
	copyOfOff := idOf(send(http.MethodPost, "/projects/"+itoa(off)+"/copy", `{"name":"off copy","domain":"offcopy.localhost"}`))
	if enabled(copyOfOff) {
		t.Fatal("a copy of a project with the tests off must also have them off")
	}
	copyOfOn := idOf(send(http.MethodPost, "/projects/"+itoa(on)+"/copy", `{"name":"on copy","domain":"oncopy.localhost"}`))
	if !enabled(copyOfOn) {
		t.Fatal("a copy of a project with the tests on must have them on")
	}

	// Settings: an update with the field changes it, one without leaves it alone.
	update := func(id int, extra string) {
		t.Helper()
		rec := send(http.MethodPut, "/projects/"+itoa(id), `{"name":"n","llm_model_id":1`+extra+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("update answered %d: %s", rec.Code, rec.Body.String())
		}
	}
	update(on, `,"user_flows":false`)
	if enabled(on) {
		t.Fatal("an update with user_flows false must turn the tests off")
	}
	update(on, ``)
	if enabled(on) {
		t.Fatal("an update without the field must leave the choice alone")
	}
	update(on, `,"user_flows":true`)
	if !enabled(on) {
		t.Fatal("an update with user_flows true must turn the tests back on")
	}
}
