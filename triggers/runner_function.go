package triggers

import (
	"database/sql"
	"fmt"
	"time"

	"openfabrica/events"
	"openfabrica/internal/funcexec"
)

// executeRunFunction invokes a stored manage_function handler as a trigger
// action, so event-driven logic the declarative action types can't express (an
// external API call with response shaping, multi-step validate-then-write, a
// computed derivation) runs server-side off a data/auth/payment event. The event
// payload is handed to the handler as req.body; the handler's declared writes are
// applied by funcexec - the same execution path and guards the HTTP function
// endpoint uses. Fire-and-forget: the handler's response is discarded and only an
// execution error is logged.
//
// Config: {"endpoint_path": "process-order"}.
func (r *Runner) executeRunFunction(projectDB *sql.DB, event events.Event, config map[string]any) error {
	path, _ := config["endpoint_path"].(string)
	if path == "" {
		path, _ = config["function"].(string)
	}
	if path == "" {
		return fmt.Errorf("run_function: 'endpoint_path' is required")
	}

	var code, secrets string
	var timeoutMS int
	err := projectDB.QueryRow(
		"SELECT code, COALESCE(secrets,''), COALESCE(timeout_ms,0) FROM of_function_endpoints WHERE path = ?",
		path,
	).Scan(&code, &secrets, &timeoutMS)
	if err == sql.ErrNoRows {
		return fmt.Errorf("run_function: no function endpoint at %q", path)
	}
	if err != nil {
		return fmt.Errorf("run_function: loading %q: %w", path, err)
	}

	spec := funcexec.Spec{Code: code, SecretsJSON: secrets}
	if timeoutMS > 0 {
		spec.Timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	// A trigger invocation has no HTTP request: the event payload IS the input.
	req := map[string]any{
		"method":  "event",
		"path":    "/trigger/" + string(event.Type),
		"query":   map[string]any{},
		"headers": map[string]any{},
		"body":    event.Payload,
		"user":    nil,
	}
	// projectDB is the project's writer connection (reads work on it too), so it
	// backs both ctx.query and the declared-write transaction.
	rt := funcexec.NewRuntime(event.ProjectID, projectDB, projectDB, r.completeDeps())
	_, err = funcexec.Execute(rt, spec, req)
	return err
}
