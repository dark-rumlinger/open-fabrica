package data

import (
	"testing"
	"time"

	"openfabrica/internal/dbtest"
	"openfabrica/tools"
)

// manage_knowledge list_bases reports a chunk count per base. It must collect
// the base list before counting: ctx.DB is the single-connection writer pool, so
// a count issued while the base cursor is still open waits on a connection that
// cannot free.
func TestKnowledgeListBases_NoDeadlockOnRealWriterPool(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	if _, err := pdb.ExecWrite(
		`CREATE TABLE kb_docs (id INTEGER PRIMARY KEY AUTOINCREMENT, content TEXT)`,
	); err != nil {
		t.Fatalf("create chunk table: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO kb_docs (content) VALUES ('a'), ('b')`); err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_knowledge_bases (name, table_name, description) VALUES ('docs', 'kb_docs', 'Docs')`,
	); err != nil {
		t.Fatalf("register base: %v", err)
	}
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var res *tools.Result
	var err error
	dbtest.RunWithDeadlineGuard(t, 10*time.Second, "manage_knowledge list_bases", func() {
		res, err = knowledgeListBases(ctx)
	})
	if err != nil {
		t.Fatalf("knowledgeListBases: %v", err)
	}
	if !res.Success {
		t.Fatalf("knowledgeListBases failed: %s", res.Error)
	}

	// The count must survive the restructure, not just the absence of a hang.
	data := res.Data.(map[string]any)
	bases := data["knowledge_bases"].([]map[string]any)
	if len(bases) != 1 {
		t.Fatalf("got %d bases, want 1", len(bases))
	}
	if got := bases[0]["chunks"]; got != 2 {
		t.Errorf("chunks = %v, want 2", got)
	}
}
