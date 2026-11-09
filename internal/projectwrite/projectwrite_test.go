package projectwrite

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE players (id INTEGER PRIMARY KEY, bankroll INTEGER DEFAULT 0);
		CREATE TABLE hand_history (id INTEGER PRIMARY KEY, player_id INTEGER, net INTEGER);
		INSERT INTO players (id, bankroll) VALUES (7, 1000);`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestInsert(t *testing.T) {
	db := openDB(t)
	if _, err := Insert(db, "hand_history", map[string]any{"player_id": 7, "net": 150}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var net int
	if err := db.QueryRow("SELECT net FROM hand_history WHERE player_id = 7").Scan(&net); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if net != 150 {
		t.Fatalf("net = %d, want 150", net)
	}
}

func TestUpdateIncrement(t *testing.T) {
	db := openDB(t)
	if _, err := Update(db, "players",
		map[string]any{"bankroll": map[string]any{"$increment": 150}},
		map[string]any{"id": 7}); err != nil {
		t.Fatalf("update: %v", err)
	}
	var bankroll int
	if err := db.QueryRow("SELECT bankroll FROM players WHERE id = 7").Scan(&bankroll); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if bankroll != 1150 {
		t.Fatalf("bankroll = %d, want 1150 (1000 + 150)", bankroll)
	}
}

func TestUpdateDecrementAndSet(t *testing.T) {
	db := openDB(t)
	if _, err := Update(db, "players",
		map[string]any{"bankroll": map[string]any{"$decrement": 400}},
		map[string]any{"id": 7}); err != nil {
		t.Fatalf("decrement: %v", err)
	}
	var bankroll int
	_ = db.QueryRow("SELECT bankroll FROM players WHERE id = 7").Scan(&bankroll)
	if bankroll != 600 {
		t.Fatalf("bankroll = %d, want 600", bankroll)
	}
	if _, err := Update(db, "players",
		map[string]any{"bankroll": 42},
		map[string]any{"id": 7}); err != nil {
		t.Fatalf("set: %v", err)
	}
	_ = db.QueryRow("SELECT bankroll FROM players WHERE id = 7").Scan(&bankroll)
	if bankroll != 42 {
		t.Fatalf("bankroll = %d, want 42", bankroll)
	}
}

func TestUpsert(t *testing.T) {
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE world_edits (id INTEGER PRIMARY KEY AUTOINCREMENT, pos TEXT UNIQUE, block TEXT)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	row := func(pos, block string) map[string]any {
		return map[string]any{"pos": pos, "block": block}
	}
	// First upsert inserts the brand-new key.
	if _, err := Upsert(db, "world_edits", row("5,10,5", "stone"), []string{"pos"}); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	// Second upsert on the SAME key updates in place - the case ctx.update silently drops.
	if _, err := Upsert(db, "world_edits", row("5,10,5", "air"), []string{"pos"}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	var count int
	var block string
	_ = db.QueryRow("SELECT COUNT(*) FROM world_edits").Scan(&count)
	_ = db.QueryRow("SELECT block FROM world_edits WHERE pos = '5,10,5'").Scan(&block)
	if count != 1 || block != "air" {
		t.Fatalf("after upsert: count=%d block=%q, want 1 row and \"air\"", count, block)
	}
	// The conflict column must be part of the row (it names the key being written).
	if _, err := Upsert(db, "world_edits", map[string]any{"block": "dirt"}, []string{"pos"}); err == nil {
		t.Fatal("expected a missing conflict column to be rejected")
	}
	// Injection payloads in the conflict target are rejected, not interpolated.
	if _, err := Upsert(db, "world_edits", row("1,1,1", "x"), []string{"pos); DROP TABLE world_edits;--"}); err == nil {
		t.Fatal("expected an invalid conflict column to be rejected")
	}
	if _, err := Upsert(db, "world_edits", row("1,1,1", "x"), nil); err == nil {
		t.Fatal("expected empty conflict columns to be rejected")
	}
}

// The realtime guide's leaderboard write: upsert a per-user row whose counter starts
// at the delta and grows on every later round. The {"$increment": n} object must be
// applied as an increment, not bound as a literal value, or the leaderboard never moves.
func TestUpsertCounter(t *testing.T) {
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE leaderboard (user_id INTEGER PRIMARY KEY, wins INTEGER DEFAULT 0, name TEXT)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	win := map[string]any{"user_id": 3, "wins": map[string]any{"$increment": 1}, "name": "ada"}
	for i := 0; i < 3; i++ {
		if _, err := Upsert(db, "leaderboard", win, []string{"user_id"}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	var wins int
	_ = db.QueryRow("SELECT wins FROM leaderboard WHERE user_id = 3").Scan(&wins)
	if wins != 3 {
		t.Fatalf("wins = %d, want 3 (inserted at 1, then +1 twice)", wins)
	}
	if _, err := Upsert(db, "leaderboard",
		map[string]any{"user_id": 3, "wins": map[string]any{"$decrement": 2}}, []string{"user_id"}); err != nil {
		t.Fatalf("decrement: %v", err)
	}
	_ = db.QueryRow("SELECT wins FROM leaderboard WHERE user_id = 3").Scan(&wins)
	if wins != 1 {
		t.Fatalf("wins = %d, want 1 after $decrement 2", wins)
	}
	if _, err := Upsert(db, "leaderboard",
		map[string]any{"user_id": map[string]any{"$increment": 1}}, []string{"user_id"}); err == nil {
		t.Fatal("a counter on the conflict column must be rejected")
	}
}

func TestInvalidIdentifiersRejected(t *testing.T) {
	db := openDB(t)
	// A column name carrying an injection payload must be refused, not interpolated.
	if _, err := Insert(db, "hand_history", map[string]any{"net); DROP TABLE players;--": 1}); err == nil {
		t.Fatal("expected invalid column name to be rejected")
	}
	if _, err := Insert(db, "bad table", map[string]any{"net": 1}); err == nil {
		t.Fatal("expected invalid table name to be rejected")
	}
	if _, err := Update(db, "players", map[string]any{"bankroll": 1}, map[string]any{}); err == nil {
		t.Fatal("expected empty where to be rejected")
	}
}
