package public

import (
	"testing"

	"openfabrica/db"
)

// TestWSWriteToTableSkipsFrameMissingRequiredColumn pins the relay-mirror
// hardening: a heterogeneous relay endpoint (write_to_table set) carries frames
// that fill the durable row AND frames that don't. A frame missing a required
// NOT NULL column is SKIPPED (no insert attempt, no NOT NULL constraint error);
// a genuine frame that supplies it still persists (e.g. a broadcast that sends
// {x,y,z} but omits a required pos column).
func TestWSWriteToTableSkipsFrameMissingRequiredColumn(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`CREATE TABLE world_edits (id INTEGER PRIMARY KEY AUTOINCREMENT, pos TEXT NOT NULL, x INTEGER, y INTEGER, z INTEGER, block_type TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ws := &wsEndpointConfig{
		WriteToTable:    "world_edits",
		tableColumns:    map[string]bool{"pos": true, "x": true, "y": true, "z": true, "block_type": true},
		requiredColumns: map[string]bool{"pos": true},
	}
	count := func() int {
		var n int
		if err := pdb.QueryRow("SELECT COUNT(*) FROM world_edits").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// A broadcast frame with coords but no pos is skipped (no row, no constraint error).
	wsWriteToTable(pdb, ws, map[string]any{"type": "break", "x": 1, "y": 2, "z": 3}, 0, 0, "")
	if got := count(); got != 0 {
		t.Fatalf("a frame missing required column pos should be skipped; got %d rows, want 0", got)
	}
	// A genuine edit frame carrying pos still persists.
	wsWriteToTable(pdb, ws, map[string]any{"pos": "1,2,3", "x": 1, "y": 2, "z": 3, "block_type": "stone"}, 0, 0, "")
	if got := count(); got != 1 {
		t.Fatalf("an edit frame with pos should persist; got %d rows, want 1", got)
	}
	var pos, block string
	if err := pdb.QueryRow("SELECT pos, block_type FROM world_edits").Scan(&pos, &block); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if pos != "1,2,3" || block != "stone" {
		t.Fatalf("persisted row = pos=%q block=%q, want 1,2,3 / stone", pos, block)
	}
}

// TestWSWriteToTableStampsServerRoom pins that the persisted room comes from the
// server-tracked room, not the client frame. A client legitimately in room "A"
// must not persist a row tagged room "B" by putting room:"B" in the payload
// (room-gated endpoints disable in-band switching, but the client still controls
// the frame's room field).
func TestWSWriteToTableStampsServerRoom(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`CREATE TABLE chat (id INTEGER PRIMARY KEY AUTOINCREMENT, room TEXT, body TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ws := &wsEndpointConfig{
		WriteToTable: "chat",
		RoomColumn:   "room",
		tableColumns: map[string]bool{"room": true, "body": true},
	}
	// Client claims room "B" in the frame, but the connection is tracked in room "A".
	wsWriteToTable(pdb, ws, map[string]any{"room": "B", "body": "hi"}, 0, 0, "A")
	var room string
	if err := pdb.QueryRow("SELECT room FROM chat").Scan(&room); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if room != "A" {
		t.Fatalf("persisted room = %q, want server-tracked A (client must not spoof the room)", room)
	}
}
