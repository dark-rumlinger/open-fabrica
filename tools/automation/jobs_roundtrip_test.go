package automation

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/tools/tooltest"
)

// Enqueue, read back, and list a job against the migrated schema. job_status and
// list_jobs select of_jobs.result/error, which the migrated table must provide, or
// every job reads back as "not found" and every list fails.
func TestJobRoundTripOnMigratedSchema(t *testing.T) {
	h := tooltest.New(t)
	res := h.MustOK("manage_schedules", map[string]any{
		"action": "enqueue_job", "type": "http_request",
		"payload": `{"url":"https://example.com","method":"GET"}`,
	})
	data, _ := res.Data.(map[string]any)
	id, ok := data["id"]
	if !ok {
		id, ok = data["job_id"]
	}
	if !ok {
		t.Fatalf("enqueue_job returned no id: %s", tooltest.DataJSON(res))
	}
	// Round-trip the id through JSON: the model sends numbers as JSON floats.
	var idArg float64
	b, _ := json.Marshal(id)
	_ = json.Unmarshal(b, &idArg)
	status := h.MustOK("manage_schedules", map[string]any{"action": "job_status", "id": idArg})
	if !strings.Contains(tooltest.DataJSON(status), "pending") {
		t.Fatalf("job_status should report the queued job: %s", tooltest.DataJSON(status))
	}
	list := h.MustOK("manage_schedules", map[string]any{"action": "list_jobs"})
	if !strings.Contains(tooltest.DataJSON(list), "http_request") {
		t.Fatalf("list_jobs does not show the enqueued job: %s", tooltest.DataJSON(list))
	}
}

// A prompt added to an existing schedule whose cadence is left untouched must still
// meet the hourly LLM floor: the guard used to run only when the cadence changed.
func TestUpdateSchedulePromptChecksStoredCadence(t *testing.T) {
	h := tooltest.New(t)
	if _, err := h.DB().Exec(
		`INSERT INTO of_scheduled_tasks (name, interval_seconds, is_enabled) VALUES ('fast', 60, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := h.Call("manage_schedules", map[string]any{
		"action": "update_schedule", "id": float64(1), "prompt": "summarize the new orders",
	})
	if res.Success {
		t.Fatalf("adding a prompt to a 60s schedule must be refused, got %s", tooltest.DataJSON(res))
	}
	if !strings.Contains(res.Error, "at most once per hour") {
		t.Fatalf("want the hourly-floor reason, got %q", res.Error)
	}
}
