package admin

import (
	"regexp"
	"strings"
	"testing"

	"openfabrica/db"
)

// Every column scanLLMCall reads must exist on of_llm_calls. Checks the reader's
// SELECT list against the real migrated project schema (PRAGMA table_info), so a
// reader naming a dropped or renamed column fails here instead of at the first
// build export. Replaces an older guard that parsed the writer's Go source as text.
func TestLLMCallColumnsMatchSchema(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	cols := map[string]bool{}
	rows, err := pdb.Query(`PRAGMA table_info(of_llm_calls)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}

	// Each SELECT part is a bare column or a COALESCE(col, <literal>) wrapper; pull
	// the column name and skip the literal-fallback fragments (0), '', etc.).
	colRe := regexp.MustCompile(`(?:COALESCE\()?([a-z_]+)`)
	for _, part := range strings.Split(llmCallColumns, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m := colRe.FindStringSubmatch(part)
		if m == nil || m[1] == "coalesce" {
			continue
		}
		if !cols[m[1]] {
			t.Errorf("llmCallColumns reads %q but of_llm_calls has no such column - update the reader or the schema", m[1])
		}
	}
}
