package admin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// llmCallColumns must stay a subset of what the writer actually inserts (plus
// the table's own id/created_at), or the reader breaks the day the log gains a
// column one side forgets. Reads the writer's source as text, the same way
// runtime_frames_drift_test pins sibling packages.
func TestLLMCallColumnsMatchWriter(t *testing.T) {
	b, err := os.ReadFile("../../providers/llm/db_logger.go")
	if err != nil {
		t.Fatalf("read db_logger.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "INSERT INTO of_llm_calls")
	if i < 0 {
		t.Fatal("db_logger.go lost its of_llm_calls INSERT; update this guard's source pointer")
	}
	stmt := src[i:]
	if j := strings.Index(stmt, ") VALUES"); j >= 0 {
		stmt = stmt[:j]
	}
	written := map[string]bool{"id": true, "created_at": true} // table-managed
	for _, col := range regexp.MustCompile(`[a-z_]+`).FindAllString(stmt, -1) {
		written[col] = true
	}

	colRe := regexp.MustCompile(`(?:COALESCE\()?([a-z_]+)`)
	for _, part := range strings.Split(llmCallColumns, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "0)" || part == "''" || part == "''\\)" {
			continue
		}
		m := colRe.FindStringSubmatch(part)
		if m == nil || m[1] == "coalesce" {
			continue
		}
		if !written[m[1]] {
			t.Errorf("llmCallColumns reads %q but the writer's INSERT never sets it - add it to "+
				"db_logger.go's column list or drop it from the reader", m[1])
		}
	}
}
