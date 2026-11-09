package public

import (
	"testing"

	"openfabrica/db"
)

func ttlRowCount(t *testing.T, pdb *db.ProjectDB) int {
	t.Helper()
	var n int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestPruneExpiredMessages pins age-based "disappearing messages": rows older
// than MessageTTLSeconds are swept, fresh rows survive, a zero TTL / missing
// table / missing created_at column is a no-op, and a tenant column scopes the
// sweep. This is the age-based counterpart to pruneEphemeralRoom (occupancy-based).
func TestPruneExpiredMessages(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll) // release file handles before TempDir removal (Windows)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, tenant_id INTEGER, content TEXT, created_at DATETIME)`,
	); err != nil {
		t.Fatalf("create messages table: %v", err)
	}
	seed := func() {
		if _, err := pdb.ExecWrite("DELETE FROM messages"); err != nil {
			t.Fatalf("reset rows: %v", err)
		}
		if _, err := pdb.ExecWrite(
			`INSERT INTO messages (tenant_id, content, created_at) VALUES
			 (0, 'old', datetime('now','-3600 seconds')), (0, 'fresh', datetime('now'))`,
		); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
	}

	t.Run("sweeps rows past the TTL, keeps fresh", func(t *testing.T) {
		seed()
		pruneExpiredMessages(pdb, &wsEndpointConfig{MessageTTLSeconds: 60, WriteToTable: "messages"}, 0)
		if got := ttlRowCount(t, pdb); got != 1 {
			t.Fatalf("after sweep want 1 row, got %d", got)
		}
		var content string
		pdb.QueryRow("SELECT content FROM messages").Scan(&content)
		if content != "fresh" {
			t.Errorf("kept the wrong row: %q, want \"fresh\"", content)
		}
	})

	t.Run("TTL=0 and missing table are no-ops", func(t *testing.T) {
		seed()
		pruneExpiredMessages(pdb, &wsEndpointConfig{MessageTTLSeconds: 0, WriteToTable: "messages"}, 0)
		pruneExpiredMessages(pdb, &wsEndpointConfig{MessageTTLSeconds: 60}, 0) // no write_to_table
		if got := ttlRowCount(t, pdb); got != 2 {
			t.Errorf("no-op calls changed rows: got %d, want 2", got)
		}
	})

	t.Run("missing created_at column is guarded", func(t *testing.T) {
		seed()
		ws := &wsEndpointConfig{MessageTTLSeconds: 60, WriteToTable: "messages", tableColumns: map[string]bool{"content": true}}
		pruneExpiredMessages(pdb, ws, 0)
		if got := ttlRowCount(t, pdb); got != 2 {
			t.Errorf("guard failed (table without created_at swept): got %d, want 2", got)
		}
	})

	t.Run("tenant column scopes the sweep", func(t *testing.T) {
		if _, err := pdb.ExecWrite("DELETE FROM messages"); err != nil {
			t.Fatalf("reset rows: %v", err)
		}
		if _, err := pdb.ExecWrite(
			`INSERT INTO messages (tenant_id, content, created_at) VALUES
			 (1, 'old1', datetime('now','-3600 seconds')), (2, 'old2', datetime('now','-3600 seconds'))`,
		); err != nil {
			t.Fatalf("seed tenant rows: %v", err)
		}
		ws := &wsEndpointConfig{
			MessageTTLSeconds: 60, WriteToTable: "messages", TenantColumn: "tenant_id",
			tableColumns: map[string]bool{"tenant_id": true, "content": true, "created_at": true},
		}
		pruneExpiredMessages(pdb, ws, 1)
		var t1, t2 int
		pdb.QueryRow("SELECT COUNT(*) FROM messages WHERE tenant_id = 1").Scan(&t1)
		pdb.QueryRow("SELECT COUNT(*) FROM messages WHERE tenant_id = 2").Scan(&t2)
		if t1 != 0 {
			t.Errorf("tenant 1 stale row not pruned: count = %d, want 0", t1)
		}
		if t2 != 1 {
			t.Errorf("tenant 2 row should survive the scoped sweep: count = %d, want 1", t2)
		}
	})
}
