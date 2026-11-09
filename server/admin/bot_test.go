package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/bot"
	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
)

func newBotTestDeps(t *testing.T) *Deps {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })
	pm := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(func() { pm.CloseAll() })
	enc, err := security.NewEncryptor(strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	svc := bot.NewBotService(&bot.BotDeps{
		DB: gdb, ProjectDBManager: pm, Bus: bus, Encryptor: enc, Logger: slog.Default(),
	})
	return &Deps{Core: sdeps.Core{
		DB: gdb, ProjectDBManager: pm, Encryptor: enc, Bus: bus, Logger: slog.Default(), Bot: svc,
	}}
}

func doJSON(h http.HandlerFunc, method, path, body string, params map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if len(params) > 0 {
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		req = req.WithContext(withChiCtx(req, rc))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestSettingsMaskAndKeepBotToken(t *testing.T) {
	d := newBotTestDeps(t)
	h := &SettingsHandler{deps: d}

	if err := bot.SaveBotToken(d.DB, d.Encryptor, "123:SECRET"); err != nil {
		t.Fatal(err)
	}

	rec := doJSON(h.Get, "GET", "/settings", "", nil)
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got[bot.TokenSettingKey] != bot.TokenMask || got[bot.TokenSettingKey+"_set"] != "1" {
		t.Fatalf("token not masked: %v", got)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatal("the token leaked into the settings response")
	}

	// Saving the form with the mask must not overwrite the real token.
	body := `{"telegram_bot_token":"` + bot.TokenMask + `","telegram_bot_token_set":"1","other":"x"}`
	if rec := doJSON(h.Update, "PUT", "/settings", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", rec.Code, rec.Body.String())
	}
	if tok := bot.LoadBotToken(d.DB, d.Encryptor); tok != "123:SECRET" {
		t.Errorf("token after masked save = %q", tok)
	}
	if v, _ := d.DB.GetSetting("telegram_bot_token_set"); v != "" {
		t.Error("derived flag must not be stored")
	}

	// A real new token is stored encrypted.
	doJSON(h.Update, "PUT", "/settings", `{"telegram_bot_token":"456:NEW"}`, nil)
	raw, _ := d.DB.GetSetting(bot.TokenSettingKey)
	if strings.Contains(raw, "456:NEW") {
		t.Error("new token stored in plaintext")
	}
	if tok := bot.LoadBotToken(d.DB, d.Encryptor); tok != "456:NEW" {
		t.Errorf("token = %q", tok)
	}

	// Clearing the field clears the setting.
	doJSON(h.Update, "PUT", "/settings", `{"telegram_bot_token":""}`, nil)
	if tok := bot.LoadBotToken(d.DB, d.Encryptor); tok != "" {
		t.Errorf("token not cleared: %q", tok)
	}
}

func TestBotStatusAndUsers(t *testing.T) {
	d := newBotTestDeps(t)
	h := &BotHandler{deps: d}

	rec := doJSON(h.Status, "GET", "/telegram/status", "", nil)
	var st bot.ConnectionStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Configured || len(st.Users) != 0 {
		t.Fatalf("fresh install status = %+v", st)
	}

	// Pairing needs a saved token.
	if rec := doJSON(h.Pair, "POST", "/telegram/pair", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("pair without token = %d, want 400", rec.Code)
	}

	_ = d.Bot.AddUser("1", "Ann")
	_ = d.Bot.AddUser("2", "Bo")

	rec = doJSON(h.RemoveUser, "DELETE", "/telegram/users/1", "", map[string]string{"userID": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(h.RemoveUser, "DELETE", "/telegram/users/9", "", map[string]string{"userID": "9"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown user = %d, want 400", rec.Code)
	}
	if users := d.Bot.Users(); len(users) != 1 || users[0].ID != "2" {
		t.Errorf("users after changes = %+v", users)
	}

	// Test message to someone who is not connected is refused.
	rec = doJSON(h.SendTest, "POST", "/telegram/test-message", `{"user_id":"77"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("test message to stranger = %d, want 400", rec.Code)
	}
}

func withChiCtx(req *http.Request, rc *chi.Context) context.Context {
	return context.WithValue(req.Context(), chi.RouteCtxKey, rc)
}

// When the token-budget env var is set it shadows the saved setting, so Get
// reports it for the page to explain; the derived key is never stored.
func TestSettingsReportsBudgetEnvOverride(t *testing.T) {
	d := newBotTestDeps(t)
	h := &SettingsHandler{deps: d}

	rec := doJSON(h.Get, "GET", "/settings", "", nil)
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if _, ok := got[budgetEnvSettingKey]; ok {
		t.Fatalf("override key present without the env var: %v", got)
	}

	t.Setenv(budgetEnvVar, "250000")
	rec = doJSON(h.Get, "GET", "/settings", "", nil)
	got = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got[budgetEnvSettingKey] != "250000" {
		t.Fatalf("override = %q, want 250000", got[budgetEnvSettingKey])
	}

	doJSON(h.Update, "PUT", "/settings", `{"`+budgetEnvSettingKey+`":"1"}`, nil)
	if v, _ := d.DB.GetSetting(budgetEnvSettingKey); v != "" {
		t.Error("derived override key must not be stored")
	}
}

// A secret whose value was blanked (an imported project) is flagged so the UI can
// ask for it; the value itself is never returned.
func TestSecretsListFlagsEmptyValues(t *testing.T) {
	d := newBotTestDeps(t)
	pdb, err := d.ProjectDBManager.Open(5)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO of_secrets (name, value_encrypted) VALUES ('filled', 'cipher-blob')`,
		`INSERT INTO of_secrets (name, value_encrypted) VALUES ('blank', '')`,
	} {
		if _, err := pdb.Writer().Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	h := &ProjectSecretsHandler{deps: d}
	rec := doJSON(h.List, "GET", "/projects/5/secrets", "", map[string]string{"projectID": "5"})
	if strings.Contains(rec.Body.String(), "cipher-blob") {
		t.Fatal("the stored value leaked into the list")
	}
	var got []secretEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	has := map[string]bool{}
	for _, s := range got {
		has[s.Name] = s.HasValue
	}
	if len(has) != 2 || !has["filled"] || has["blank"] {
		t.Errorf("has_value = %v, want filled=true blank=false", has)
	}
}
