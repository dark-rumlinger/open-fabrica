package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
)

// newSetupTestHandler builds an AuthHandler over a real migrated database with
// NO users, which is the only state the first-run endpoints serve.
func newSetupTestHandler(t *testing.T) (*AuthHandler, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return &AuthHandler{deps: &Deps{Core: sdeps.Core{
		DB:     database,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}}, database
}

func seedTemplate(t *testing.T, database *db.DB, vendor, plan string) {
	t.Helper()
	if _, err := models.UpsertProviderTemplate(database.DB, models.ProviderTemplate{
		Vendor: vendor, Name: vendor, Transport: "openai-chat", Kind: "llm", Plan: plan,
		BaseURL: "http://127.0.0.1:0/v1", RequiresAPIKey: true, ConfigJSON: "{}",
	}); err != nil {
		t.Fatalf("seed %s: %v", vendor, err)
	}
}

// The wizard tells metered and subscription endpoints apart by the plan field,
// so every row it lists must carry one.
func TestSetupProviderCatalog_ReportsPlan(t *testing.T) {
	h, database := newSetupTestHandler(t)
	seedTemplate(t, database, "metered", "")
	seedTemplate(t, database, "subscription", models.PlanCoding)

	rec := httptest.NewRecorder()
	h.HandleProviderCatalog(rec, httptest.NewRequest(http.MethodGet, "/setup/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got []setupProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	plans := map[string]string{}
	for _, p := range got {
		plans[p.Vendor] = p.Plan
	}
	if plans["metered"] != models.PlanStandard || plans["subscription"] != models.PlanCoding {
		t.Errorf("plans = %v, want metered=standard subscription=coding", plans)
	}
}

// The first-run endpoints are unauthenticated, so once the box is claimed they
// must refuse. This guard is the only thing between them and anyone who can
// reach the admin port on a running instance.
func TestSetupEndpoints_CloseAfterSetup(t *testing.T) {
	h, database := newSetupTestHandler(t)

	hash, err := security.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := models.CreateUser(database.DB, "admin", hash, "admin", "Admin"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := httptest.NewRecorder()
	h.HandleProviderCatalog(rec, httptest.NewRequest(http.MethodGet, "/setup/providers", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 once an admin account exists", rec.Code)
	}
}
