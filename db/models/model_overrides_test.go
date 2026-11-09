package models_test

import (
	"path/filepath"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
)

func newProviderWithModel(t *testing.T) (*db.DB, int, int) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := "https://api.example.com/v1"
	p, err := models.CreateProvider(database.DB, "Test Co", "test-co", "openai-chat", nil, &base)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	// Arrive the way a real row does: written by a sync from the catalog.
	if _, err := models.SyncProviderModels(database.DB, p.ID, []models.SyncedModel{{
		ID: "m1", DisplayName: "Model One",
		ContextTokens: 128000, MaxOutputTokens: 8000,
		SupportsTools: true, CostInput: 1.0, CostOutput: 2.0,
	}}, true); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	rows, err := models.ListModelsByProvider(database.DB, p.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 model, got %v (%v)", len(rows), err)
	}
	return database, p.ID, rows[0].ID
}

func mustModel(t *testing.T, d *db.DB, id int) *models.LLMModel {
	t.Helper()
	m, err := models.GetModelByID(d.DB, id)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	return m
}

// The point of an admin correction is that it STICKS. A sync rewrites every
// column it manages, so without the override record an edited price would
// survive exactly until the next "Sync Models" click - which is the same as not
// being editable at all.
func TestModelOverrides_SurviveASync(t *testing.T) {
	d, providerID, modelPK := newProviderWithModel(t)

	corrected := 0.25
	if err := models.MergeModelOverrides(d.DB, modelPK, "", models.ModelOverrides{CostInput: &corrected}); err != nil {
		t.Fatalf("record override: %v", err)
	}
	// The column itself is set by the same request in the real handler.
	if err := models.UpdateModelCapabilities(d.DB, modelPK, models.ModelEdit{
		DisplayName: "Model One", ContextTokens: 128000, MaxOutputTokens: 8000,
		SupportsTools: true, CostInput: corrected, CostOutput: 2.0, IsEnabled: true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// A later sync brings the catalog's price back, and must not win.
	if _, err := models.SyncProviderModels(d.DB, providerID, []models.SyncedModel{{
		ID: "m1", DisplayName: "Model One",
		ContextTokens: 200000, MaxOutputTokens: 9000,
		SupportsTools: true, CostInput: 1.0, CostOutput: 3.0,
	}}, false); err != nil {
		t.Fatalf("resync: %v", err)
	}

	got := mustModel(t, d, modelPK)
	if got.CostInput != corrected {
		t.Errorf("cost_input = %v, want the corrected %v - the sync overwrote an admin edit", got.CostInput, corrected)
	}
	// Everything NOT corrected must still refresh, or one correction would
	// freeze the whole row at the day it was made.
	if got.ContextTokens != 200000 {
		t.Errorf("context_tokens = %d, want 200000 refreshed from the catalog", got.ContextTokens)
	}
	if got.CostOutput != 3.0 {
		t.Errorf("cost_output = %v, want 3.0 refreshed from the catalog", got.CostOutput)
	}
}

// The UI marks which numbers are the admin's, so the record has to name them.
func TestModelOverrides_ReportsWhichFields(t *testing.T) {
	d, _, modelPK := newProviderWithModel(t)

	price, reasoning := 0.5, true
	if err := models.MergeModelOverrides(d.DB, modelPK, "", models.ModelOverrides{
		CostInput: &price, SupportsReasoning: &reasoning,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := mustModel(t, d, modelPK).Overrides().OverriddenFields()
	want := []string{"cost_input", "supports_reasoning"}
	if len(got) != len(want) {
		t.Fatalf("overridden = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("overridden = %v, want %v", got, want)
		}
	}
}

// Corrections accumulate across separate edits rather than replacing each other.
func TestModelOverrides_Accumulate(t *testing.T) {
	d, _, modelPK := newProviderWithModel(t)

	first := 0.5
	if err := models.MergeModelOverrides(d.DB, modelPK, "", models.ModelOverrides{CostInput: &first}); err != nil {
		t.Fatalf("first: %v", err)
	}
	cfg := mustModel(t, d, modelPK).Config
	second := 4.0
	if err := models.MergeModelOverrides(d.DB, modelPK, cfg, models.ModelOverrides{CostOutput: &second}); err != nil {
		t.Fatalf("second: %v", err)
	}

	ov := mustModel(t, d, modelPK).Overrides()
	if ov.CostInput == nil || *ov.CostInput != first {
		t.Errorf("the first correction was lost: %+v", ov)
	}
	if ov.CostOutput == nil || *ov.CostOutput != second {
		t.Errorf("the second correction did not land: %+v", ov)
	}
}

// "Reset to catalog" hands the row back: the record clears, and the next sync
// is free to rewrite the column.
func TestModelOverrides_ResetLetsTheCatalogWinAgain(t *testing.T) {
	d, providerID, modelPK := newProviderWithModel(t)

	corrected := 0.25
	if err := models.MergeModelOverrides(d.DB, modelPK, "", models.ModelOverrides{CostInput: &corrected}); err != nil {
		t.Fatalf("record: %v", err)
	}
	cfg := mustModel(t, d, modelPK).Config
	if err := models.ClearModelOverrides(d.DB, modelPK, cfg); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if fields := mustModel(t, d, modelPK).Overrides().OverriddenFields(); len(fields) != 0 {
		t.Fatalf("overridden = %v after reset, want none", fields)
	}

	if _, err := models.SyncProviderModels(d.DB, providerID, []models.SyncedModel{{
		ID: "m1", DisplayName: "Model One", ContextTokens: 128000, CostInput: 1.0,
	}}, false); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got := mustModel(t, d, modelPK).CostInput; got != 1.0 {
		t.Errorf("cost_input = %v, want the catalog's 1.0 back after a reset", got)
	}
}

// A mangled config blob must not take a working model out of service.
func TestModelOverrides_MalformedConfigIsIgnored(t *testing.T) {
	d, _, modelPK := newProviderWithModel(t)
	if _, err := d.DB.Exec("UPDATE llm_models SET config = ? WHERE id = ?", "{not json", modelPK); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if fields := mustModel(t, d, modelPK).Overrides().OverriddenFields(); len(fields) != 0 {
		t.Errorf("overridden = %v, want none from a malformed blob", fields)
	}
}
