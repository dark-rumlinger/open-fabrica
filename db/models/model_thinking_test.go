package models_test

import (
	"testing"

	"openfabrica/db/models"
)

// The stage map shares the config blob with the admin's catalog corrections; each
// write keeps the other, and a reset of the corrections leaves the map alone.
func TestModelThinking_SharesConfigWithOverrides(t *testing.T) {
	d, _, modelPK := newProviderWithModel(t)

	corrected := 0.25
	if err := models.MergeModelOverrides(d.DB, modelPK, "", models.ModelOverrides{CostInput: &corrected}); err != nil {
		t.Fatalf("record override: %v", err)
	}
	if err := models.SetModelThinking(d.DB, modelPK, mustModel(t, d, modelPK).Config, map[string]bool{"plan": true, "build": false}); err != nil {
		t.Fatalf("set thinking: %v", err)
	}
	m := mustModel(t, d, modelPK)
	if got := m.Thinking(); !got["plan"] || got["build"] || len(got) != 2 {
		t.Fatalf("thinking = %v, want plan on and build off", got)
	}
	if fields := m.Overrides().OverriddenFields(); len(fields) != 1 {
		t.Fatalf("overridden = %v, want the cost correction kept", fields)
	}

	if err := models.ClearModelOverrides(d.DB, modelPK, m.Config); err != nil {
		t.Fatalf("clear overrides: %v", err)
	}
	if got := mustModel(t, d, modelPK).Thinking(); !got["plan"] {
		t.Fatalf("thinking = %v after an overrides reset, want it kept", got)
	}

	if err := models.SetModelThinking(d.DB, modelPK, mustModel(t, d, modelPK).Config, nil); err != nil {
		t.Fatalf("clear thinking: %v", err)
	}
	if got := mustModel(t, d, modelPK).Thinking(); got != nil {
		t.Fatalf("thinking = %v after clearing, want none", got)
	}
}

func TestModelThinking_RejectsUnknownStage(t *testing.T) {
	d, _, modelPK := newProviderWithModel(t)
	if err := models.SetModelThinking(d.DB, modelPK, "", map[string]bool{"deploy": true}); err == nil {
		t.Fatal("want an error for a stage the engine does not have")
	}
}

// Probe verdicts set their columns and sit beside the thinking map.
func TestProbeVerdict_StoredWithThinking(t *testing.T) {
	d, _, modelPK := newProviderWithModel(t)
	if _, probed := mustModel(t, d, modelPK).ProbeVerdict("reasoning"); probed {
		t.Fatal("a fresh model has no verdict")
	}
	if err := models.SetModelThinking(d.DB, modelPK, "", map[string]bool{"plan": true}); err != nil {
		t.Fatal(err)
	}
	if err := models.SetProbeVerdict(d.DB, modelPK, mustModel(t, d, modelPK).Config, "reasoning", true); err != nil {
		t.Fatal(err)
	}
	if err := models.SetProbeVerdict(d.DB, modelPK, mustModel(t, d, modelPK).Config, "tools", false); err != nil {
		t.Fatal(err)
	}
	m := mustModel(t, d, modelPK)
	if has, probed := m.ProbeVerdict("reasoning"); !probed || !has || !m.SupportsReasoning {
		t.Fatalf("reasoning verdict = %v/%v, column = %v, want a stored yes", has, probed, m.SupportsReasoning)
	}
	if has, probed := m.ProbeVerdict("tools"); !probed || has || m.SupportsTools {
		t.Fatalf("tools verdict = %v/%v, column = %v, want a stored no", has, probed, m.SupportsTools)
	}
	if !m.Thinking()["plan"] {
		t.Fatal("verdict writes must keep the thinking map")
	}
	if err := models.SetProbeVerdict(d.DB, modelPK, m.Config, "vision", true); err == nil {
		t.Fatal("an unknown capability must be refused")
	}
}
