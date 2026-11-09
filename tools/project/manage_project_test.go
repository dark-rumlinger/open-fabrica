package project

import (
	"encoding/json"
	"testing"

	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func infoData(t *testing.T, h *tooltest.Harness) map[string]any {
	t.Helper()
	res := h.MustOK("manage_project", map[string]any{"action": "info"})
	var out map[string]any
	if err := json.Unmarshal([]byte(tooltest.DataJSON(res)), &out); err != nil {
		t.Fatalf("info data is not an object: %v", err)
	}
	return out
}

// info reports the phase and whether the project is published, the two things an
// owner sees, rather than the stored mode and status columns.
func TestProjectInfoReportsPhaseAndPublished(t *testing.T) {
	h := tooltest.New(t)
	if _, err := h.Global.Exec(
		`INSERT INTO projects (id, name, published, mode, llm_model_id) VALUES (1, 'Acme', 0, 'live', 1)`); err != nil {
		t.Fatal(err)
	}

	got := infoData(t, h)
	phase, _ := got["phase"].(map[string]any)
	if phase["key"] != "live" || phase["label"] != "Live" {
		t.Errorf("phase = %v, want live", got["phase"])
	}
	if got["published"] != false {
		t.Errorf("published = %v, want false", got["published"])
	}
	if _, ok := got["mode"]; ok {
		t.Error("info still exposes the stored mode column")
	}
}

func TestProjectInfoSeesAParkedRunAsNeedsYou(t *testing.T) {
	h := tooltest.New(t)
	if _, err := h.Global.Exec(
		`INSERT INTO projects (id, name, published, mode, llm_model_id) VALUES (1, 'Acme', 1, 'building', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB().Exec(`INSERT INTO of_runs (id, status, stage, pause_reason)
		VALUES ('r1', 'paused', 'plan', 'awaiting_owner_answers')`); err != nil {
		t.Fatal(err)
	}

	phase, _ := infoData(t, h)["phase"].(map[string]any)
	if phase["key"] != "needs_you" {
		t.Errorf("phase = %v, want needs_you", phase)
	}
}

// Only the owner and the engine move a project between phases: a write from a
// tool would reach neither the running worker nor the admin UI.
func TestProjectToolCannotChangeThePhase(t *testing.T) {
	def, err := tools.DefaultRegistry().Get("manage_project")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := def.Actions["set_mode"]; ok {
		t.Fatal("manage_project still offers set_mode")
	}
	h := tooltest.New(t)
	h.MustFail("manage_project", map[string]any{"action": "set_mode", "mode": "live"}, "")
}
