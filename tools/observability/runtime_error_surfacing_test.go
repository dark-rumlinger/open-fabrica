package observability

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupRuntimeErrorDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupRuntimeErrorDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

// The gap this closes: over HTTP the probe sees only the generic body a function
// endpoint answers with, which says a call failed but not why. The recorded
// cause must reach the probe result, otherwise narrowing a 500 means rewriting
// the handler and probing again until the failure moves.
func TestServerErrorCause_ReportsRecordedCause(t *testing.T) {
	h := setupRuntimeErrorDB(t)
	thrown := "handler threw: TypeError: could not convert id = 1 to map[string]interface {}"
	db.RecordRuntimeError(h, db.RuntimeErrorFunction, "vote", thrown)

	if got := serverErrorCause(h, "vote", 500); got != thrown {
		t.Errorf("cause = %q, want %q", got, thrown)
	}
}

// Only a server error has a recorded cause behind it. A 4xx is the handler
// answering deliberately, so a stale row must not be attached to it.
func TestServerErrorCause_OnlyForServerErrors(t *testing.T) {
	h := setupRuntimeErrorDB(t)
	db.RecordRuntimeError(h, db.RuntimeErrorFunction, "vote", "old failure")

	for _, status := range []int{200, 400, 401, 404, 429} {
		if got := serverErrorCause(h, "vote", status); got != "" {
			t.Errorf("status %d must carry no handler cause, got %q", status, got)
		}
	}
}

// A 500 from an endpoint that recorded nothing (a CRUD route, or a failure
// raised outside the sandbox) reports no cause rather than another path's.
func TestServerErrorCause_SilentWithoutARecord(t *testing.T) {
	h := setupRuntimeErrorDB(t)
	db.RecordRuntimeError(h, db.RuntimeErrorFunction, "vote", "vote failure")

	if got := serverErrorCause(h, "memberships", 500); got != "" {
		t.Errorf("want no cause for an endpoint that recorded none, got %q", got)
	}
}

// The errors action is where the model looks when a 500 is unexplained, so the
// runtime causes have to be in its answer, not behind a separate action.
func TestDiagErrors_IncludesRuntimeCauses(t *testing.T) {
	h := setupRuntimeErrorDB(t)
	db.RecordRuntimeError(h, db.RuntimeErrorFunction, "vote", "handler threw: TypeError")
	if _, err := h.Exec(
		`INSERT INTO of_run_log (kind, ref, payload) VALUES ('stage','build','{"status":"failed","error":"stage blew up"}')`,
	); err != nil {
		t.Fatalf("seeding run log: %v", err)
	}

	res, err := (&DiagErrors{}).Run(&tools.ToolContext{DB: h})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("errors action failed: %s", res.Error)
	}
	body, _ := json.Marshal(res.Data)
	for _, want := range []string{"stage blew up", "handler threw: TypeError", "vote"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("errors output missing %q: %s", want, body)
		}
	}
}
