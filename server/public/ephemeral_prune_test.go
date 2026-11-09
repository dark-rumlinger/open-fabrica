package public

import (
	"testing"

	"openfabrica/db"
)

// roomRowCount counts persisted rows for a room in the test messages table.
func roomRowCount(t *testing.T, pdb *db.ProjectDB, room string) int {
	t.Helper()
	var n int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM messages WHERE room_code = ?", room).Scan(&n); err != nil {
		t.Fatalf("counting rows for room %q: %v", room, err)
	}
	return n
}

// TestPruneEphemeralRoom pins the relay-driven cleanup: an ephemeral endpoint's
// rows for a room are deleted when that room empties, a non-ephemeral endpoint
// leaves them, and other rooms (and other tenants) are never touched. This is
// the primitive that lets throwaway sessions clean up server-side so a generated
// app never reaches for a client-driven delete.
func TestPruneEphemeralRoom(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll) // release file handles before TempDir removal (Windows)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, room_code TEXT, tenant_id INTEGER, iv TEXT, ct TEXT)`,
	); err != nil {
		t.Fatalf("create messages table: %v", err)
	}
	seed := func() {
		if _, err := pdb.ExecWrite("DELETE FROM messages"); err != nil {
			t.Fatalf("reset rows: %v", err)
		}
		if _, err := pdb.ExecWrite(
			`INSERT INTO messages (room_code, tenant_id, iv, ct) VALUES
			 ('alpha', 0, 'i', 'c'), ('alpha', 0, 'i', 'c'), ('beta', 0, 'i', 'c')`,
		); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
	}

	ephemeral := &wsEndpointConfig{Ephemeral: true, WriteToTable: "messages", RoomColumn: "room_code"}

	t.Run("non-ephemeral is a no-op", func(t *testing.T) {
		seed()
		off := &wsEndpointConfig{Ephemeral: false, WriteToTable: "messages", RoomColumn: "room_code"}
		pruneEphemeralRoom(pdb, off, "alpha", 0)
		if got := roomRowCount(t, pdb, "alpha"); got != 2 {
			t.Errorf("non-ephemeral prune deleted rows: alpha count = %d, want 2", got)
		}
	})

	t.Run("prunes only the emptied room", func(t *testing.T) {
		seed()
		pruneEphemeralRoom(pdb, ephemeral, "alpha", 0)
		if got := roomRowCount(t, pdb, "alpha"); got != 0 {
			t.Errorf("alpha rows not pruned: count = %d, want 0", got)
		}
		if got := roomRowCount(t, pdb, "beta"); got != 1 {
			t.Errorf("beta rows should survive: count = %d, want 1", got)
		}
	})

	t.Run("guards: missing room/table/column never over-delete", func(t *testing.T) {
		seed()
		pruneEphemeralRoom(pdb, ephemeral, "", 0)                                                         // empty room name
		pruneEphemeralRoom(pdb, &wsEndpointConfig{Ephemeral: true}, "alpha", 0)                           // no write_to_table
		pruneEphemeralRoom(pdb, &wsEndpointConfig{Ephemeral: true, WriteToTable: "messages"}, "alpha", 0) // no room_column
		if got := roomRowCount(t, pdb, "alpha"); got != 2 {
			t.Errorf("guarded calls deleted rows: alpha count = %d, want 2", got)
		}
	})

	t.Run("tenant column scopes the delete", func(t *testing.T) {
		if _, err := pdb.ExecWrite("DELETE FROM messages"); err != nil {
			t.Fatalf("reset rows: %v", err)
		}
		if _, err := pdb.ExecWrite(
			`INSERT INTO messages (room_code, tenant_id, iv, ct) VALUES
			 ('alpha', 1, 'i', 'c'), ('alpha', 2, 'i', 'c')`,
		); err != nil {
			t.Fatalf("seed tenant rows: %v", err)
		}
		tenant := &wsEndpointConfig{
			Ephemeral:    true,
			WriteToTable: "messages",
			RoomColumn:   "room_code",
			TenantColumn: "tenant_id",
			tableColumns: map[string]bool{"room_code": true, "tenant_id": true, "iv": true, "ct": true},
		}
		pruneEphemeralRoom(pdb, tenant, "alpha", 1)
		var t1, t2 int
		pdb.QueryRow("SELECT COUNT(*) FROM messages WHERE tenant_id = 1").Scan(&t1)
		pdb.QueryRow("SELECT COUNT(*) FROM messages WHERE tenant_id = 2").Scan(&t2)
		if t1 != 0 {
			t.Errorf("tenant 1 alpha rows not pruned: count = %d, want 0", t1)
		}
		if t2 != 1 {
			t.Errorf("tenant 2 rows should survive: count = %d, want 1", t2)
		}
	})
}
