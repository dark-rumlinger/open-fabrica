package jobs

import (
	"context"
	"strings"
	"testing"
)

// The job worker's input validation and dispatch guards short-circuit before
// touching the project database, so they can be exercised with a zero-value
// Worker. These are the security-relevant paths (SQL guard, SSRF guard,
// required params, unknown type) and this keeps the tests hermetic.

func TestExecuteJobUnknownType(t *testing.T) {
	w := &Worker{}
	err := w.executeJob(context.Background(), 1, "does_not_exist", "{}")
	if err == nil || !strings.Contains(err.Error(), "unknown job type") {
		t.Fatalf("expected unknown job type error, got %v", err)
	}
}

func TestExecuteJobInvalidPayload(t *testing.T) {
	w := &Worker{}
	err := w.executeJob(context.Background(), 1, "run_sql", "{not valid json")
	if err == nil || !strings.Contains(err.Error(), "invalid payload") {
		t.Fatalf("expected invalid payload error, got %v", err)
	}
}

func TestExecuteRunSQLRejects(t *testing.T) {
	w := &Worker{}
	ctx := context.Background()
	cases := []struct {
		name  string
		query string
		match string
	}{
		{"empty", "   ", "required"},
		{"templating", "SELECT {{secret}}", "templating"},
		{"stacked", "SELECT 1; DROP TABLE users", "multiple statements"},
		{"non_select", "DELETE FROM users", "only SELECT/VALUES statements are allowed"},
		// A WITH-wrapped write must still be refused: EffectiveVerb looks past the CTE
		// definitions, so the leading "WITH" cannot launder a DELETE through this gate.
		{"cte_write", "WITH doomed AS (SELECT id FROM users) DELETE FROM users WHERE id IN (SELECT id FROM doomed)", "only SELECT/VALUES statements are allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := w.executeRunSQL(ctx, map[string]any{"sql": c.query})
			if err == nil || !strings.Contains(err.Error(), c.match) {
				t.Fatalf("query %q: expected error containing %q, got %v", c.query, c.match, err)
			}
		})
	}
}

func TestExecuteHTTPRequestGuards(t *testing.T) {
	w := &Worker{}
	ctx := context.Background()

	if err := w.executeHTTPRequest(ctx, map[string]any{}); err == nil || !strings.Contains(err.Error(), "'url' is required") {
		t.Fatalf("expected url-required error, got %v", err)
	}

	// A loopback IP literal is an SSRF target and must be blocked before any
	// network call (IP literal => no DNS, so the test is hermetic).
	err := w.executeHTTPRequest(ctx, map[string]any{"url": "http://127.0.0.1:9/admin"})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected SSRF-blocked error, got %v", err)
	}
}

func TestExecuteLLMPromptRequiresPrompt(t *testing.T) {
	w := &Worker{}
	err := w.executeLLMPrompt(context.Background(), 1, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected prompt-required error, got %v", err)
	}
}
