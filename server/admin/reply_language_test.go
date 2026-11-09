package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/replylang"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
)

// replyTestEnv is a migrated global database with two users and one project.
type replyTestEnv struct {
	deps      *Deps
	ada, bob  *models.User
	projectID int
}

func newReplyTestEnv(t *testing.T) *replyTestEnv {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ada, err := models.CreateUser(database.DB, "ada", "hash", "admin", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := models.CreateUser(database.DB, "bob", "hash", "member", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	res, err := database.ExecWrite(
		`INSERT INTO projects (name, description, published, mode, llm_model_id) VALUES ('P', 'd', 0, 'building', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return &replyTestEnv{
		deps: &Deps{Core: sdeps.Core{
			DB:     database,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}},
		ada: ada, bob: bob, projectID: int(id),
	}
}

// request builds a request signed in as user (nil for an anonymous one), carrying
// the interface language header when locale is not empty.
func (e *replyTestEnv) request(method, body string, user *models.User, locale string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	if locale != "" {
		r.Header.Set(replylang.Header, locale)
	}
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), security.ClaimsKey, &security.Claims{UserID: user.ID, Username: user.Username, Role: user.Role}))
	}
	return r
}

func (e *replyTestEnv) projectLanguage(t *testing.T) string {
	t.Helper()
	p, err := models.GetProjectByID(e.deps.DB.DB, e.projectID)
	if err != nil {
		t.Fatal(err)
	}
	return models.ProjectReplyLanguage(p)
}

func (e *replyTestEnv) setPref(t *testing.T, u *models.User, pref string) {
	t.Helper()
	if err := models.SetUserAssistantLanguage(e.deps.DB.DB, u.ID, pref); err != nil {
		t.Fatal(err)
	}
}

func TestRecordReplyLanguage(t *testing.T) {
	cases := []struct {
		name   string
		pref   string
		stored string // directive already on the project
		locale string
		want   string // directive on the project afterwards
	}{
		{"follows a non-English interface", "", "", "es", "es"},
		{"follows the interface when it changes", "", "es", "ru", "ru"},
		{"english interface clears a forced language", "", "es", "en", ""},
		{"a regional tag is reduced to its code", "", "", "zh-CN", "zh"},
		{"explicit language ignores the interface", "de", "", "es", "de"},
		{"match ignores the interface", replylang.Match, "", "ja", replylang.Match},
		{"explicit english is stored as english", "en", "", "es", "en"},
		{"no interface header leaves the project alone", "", "fr", "", "fr"},
		{"an unknown interface leaves the project alone", "", "fr", "xx", "fr"},
		{"explicit language needs no header", "pt", "", "", "pt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newReplyTestEnv(t)
			e.setPref(t, e.ada, c.pref)
			if c.stored != "" {
				if err := models.SetProjectReplyLanguage(e.deps.DB.DB, e.projectID, c.stored); err != nil {
					t.Fatal(err)
				}
			}
			e.deps.recordReplyLanguage(e.request("POST", "", e.ada, c.locale), e.projectID)
			if got := e.projectLanguage(t); got != c.want {
				t.Errorf("project language = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRecordReplyLanguageIgnoresAnonymousRequests(t *testing.T) {
	e := newReplyTestEnv(t)
	e.deps.recordReplyLanguage(e.request("POST", "", nil, "es"), e.projectID)
	if got := e.projectLanguage(t); got != "" {
		t.Errorf("a request with no signed-in user set the language to %q", got)
	}
}

// On a shared project the person who just acted decides the language of the reply.
func TestRecordReplyLanguageLastActorWins(t *testing.T) {
	e := newReplyTestEnv(t)
	e.setPref(t, e.bob, "ja")

	e.deps.recordReplyLanguage(e.request("POST", "", e.ada, "es"), e.projectID)
	if got := e.projectLanguage(t); got != "es" {
		t.Fatalf("after ada: %q", got)
	}
	e.deps.recordReplyLanguage(e.request("POST", "", e.bob, "en"), e.projectID)
	if got := e.projectLanguage(t); got != "ja" {
		t.Fatalf("after bob: %q", got)
	}
}

// A language is a courtesy; failing to store it must never fail or panic the
// request that carries the real action.
func TestRecordReplyLanguageSurvivesAMissingProject(t *testing.T) {
	e := newReplyTestEnv(t)
	e.deps.recordReplyLanguage(e.request("POST", "", e.ada, "es"), 99999)
	if got := e.projectLanguage(t); got != "" {
		t.Errorf("another project was touched: %q", got)
	}
}

func decodePrefs(t *testing.T, rec *httptest.ResponseRecorder) preferencesBody {
	t.Helper()
	var out preferencesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestPreferencesGetAndUpdate(t *testing.T) {
	e := newReplyTestEnv(t)
	h := &PreferencesHandler{deps: e.deps}

	rec := httptest.NewRecorder()
	h.Get(rec, e.request("GET", "", e.ada, ""))
	if rec.Code != http.StatusOK || decodePrefs(t, rec).AssistantLanguage != "" {
		t.Fatalf("default: %d %s", rec.Code, rec.Body.String())
	}

	for in, want := range map[string]string{"es": "es", " MATCH ": "match", "zh-CN": "zh", "": ""} {
		body, _ := json.Marshal(preferencesBody{AssistantLanguage: in})
		rec = httptest.NewRecorder()
		h.Update(rec, e.request("PUT", string(body), e.ada, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("update %q: %d %s", in, rec.Code, rec.Body.String())
		}
		if got := decodePrefs(t, rec).AssistantLanguage; got != want {
			t.Errorf("update %q answered %q, want %q", in, got, want)
		}
		rec = httptest.NewRecorder()
		h.Get(rec, e.request("GET", "", e.ada, ""))
		if got := decodePrefs(t, rec).AssistantLanguage; got != want {
			t.Errorf("after update %q, get answered %q, want %q", in, got, want)
		}
	}
}

func TestPreferencesRejectInvalidValues(t *testing.T) {
	e := newReplyTestEnv(t)
	h := &PreferencesHandler{deps: e.deps}
	e.setPref(t, e.ada, "fr")

	for _, body := range []string{`{"assistant_language":"klingon"}`, `{"assistant_language":"match me"}`, `not json`} {
		rec := httptest.NewRecorder()
		h.Update(rec, e.request("PUT", body, e.ada, ""))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	if got, _ := models.GetUserAssistantLanguage(e.deps.DB.DB, e.ada.ID); got != "fr" {
		t.Errorf("a rejected update changed the stored value to %q", got)
	}
}

func TestPreferencesAreOwnedByTheCaller(t *testing.T) {
	e := newReplyTestEnv(t)
	h := &PreferencesHandler{deps: e.deps}

	rec := httptest.NewRecorder()
	h.Update(rec, e.request("PUT", `{"assistant_language":"de"}`, e.ada, ""))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.Get(rec, e.request("GET", "", e.bob, ""))
	if got := decodePrefs(t, rec).AssistantLanguage; got != "" {
		t.Errorf("bob read ada's preference: %q", got)
	}
}

func TestPreferencesRequireASession(t *testing.T) {
	e := newReplyTestEnv(t)
	h := &PreferencesHandler{deps: e.deps}
	rec := httptest.NewRecorder()
	h.Get(rec, e.request("GET", "", nil, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("get: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.Update(rec, e.request("PUT", `{"assistant_language":"es"}`, nil, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("update: %d", rec.Code)
	}
}

// A stored value the server no longer recognizes must read as the default so the
// client's select is never handed something it has no option for.
func TestPreferencesGetNormalizesAStaleRow(t *testing.T) {
	e := newReplyTestEnv(t)
	e.setPref(t, e.ada, "klingon")
	h := &PreferencesHandler{deps: e.deps}
	rec := httptest.NewRecorder()
	h.Get(rec, e.request("GET", "", e.ada, ""))
	if got := decodePrefs(t, rec).AssistantLanguage; got != "" {
		t.Errorf("stale value leaked to the client: %q", got)
	}
}

// Each request that can wake the engine records the caller's language first. The
// handlers need a running engine to exercise end to end, so this pins the call
// sites: dropping one would silently leave that action replying in the wrong
// language with no failing behavior test.
func TestEveryEngineWakingHandlerRecordsTheLanguage(t *testing.T) {
	for file, want := range map[string]int{
		"chat.go":      1, // send a message
		"engine.go":    2, // start the engine, change the mode
		"projects.go":  1, // create a project (starts the first build)
		"questions.go": 1, // answer a question
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Count(src, []byte("recordReplyLanguage(")); got != want {
			t.Errorf("%s calls recordReplyLanguage %d times, want %d", file, got, want)
		}
	}
}
