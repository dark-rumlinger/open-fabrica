package flowsnap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
)

func setupProject(t *testing.T) (*db.ProjectDB, string) {
	t.Helper()
	pm := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(func() { pm.CloseAll() })
	pdb, err := pm.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	for _, q := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, status TEXT)`,
		`CREATE TABLE order_notes (id INTEGER PRIMARY KEY, order_id INTEGER REFERENCES orders(id) ON DELETE CASCADE, note TEXT)`,
		`CREATE TABLE tags (tag TEXT PRIMARY KEY, uses INTEGER)`,
		`CREATE VIRTUAL TABLE orders_fts USING fts5(name, content=orders, content_rowid=id)`,
		`CREATE TRIGGER orders_fts_ai AFTER INSERT ON orders BEGIN INSERT INTO orders_fts(rowid, name) VALUES (new.id, new.name); END`,
		`CREATE TRIGGER orders_fts_ad AFTER DELETE ON orders BEGIN INSERT INTO orders_fts(orders_fts, rowid, name) VALUES ('delete', old.id, old.name); END`,
		`CREATE TRIGGER orders_fts_au AFTER UPDATE ON orders BEGIN INSERT INTO orders_fts(orders_fts, rowid, name) VALUES ('delete', old.id, old.name); INSERT INTO orders_fts(rowid, name) VALUES (new.id, new.name); END`,
		`INSERT INTO orders (name, status) VALUES ('first latte', 'queued'), ('second mocha', 'ready'), ('third tea', 'queued')`,
		`INSERT INTO order_notes (order_id, note) VALUES (2, 'extra hot')`,
		`INSERT INTO tags (tag, uses) VALUES ('hot', 1), ('iced', 2)`,
		`INSERT INTO of_analytics (page_path, user_agent) VALUES ('/', 'Mozilla/5.0')`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
		if err := os.WriteFile(filepath.Join(dir, "uploads", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return pdb, dir
}

func dump(t *testing.T, pdb *db.ProjectDB, q string) [][]string {
	t.Helper()
	rows, err := pdb.Writer().Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = strings.TrimSpace(strings.ReplaceAll(stringify(v), "\n", " "))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// Everything a replay does to app data and files is undone, while the tables the
// engine writes during a replay keep their new rows.
func TestSnapshotRestoreUndoesTheReplay(t *testing.T) {
	pdb, dir := setupProject(t)
	ctx := context.Background()
	queries := map[string]string{
		"orders": `SELECT id, name, status FROM orders ORDER BY id`,
		"notes":  `SELECT id, order_id, note FROM order_notes ORDER BY id`,
		"tags":   `SELECT tag, uses FROM tags ORDER BY tag`,
		"seq":    `SELECT seq FROM sqlite_sequence WHERE name = 'orders'`,
		"fts":    `SELECT rowid FROM orders_fts WHERE orders_fts MATCH 'mocha OR tea OR flat' ORDER BY rowid`,
		"views":  `SELECT COUNT(*) FROM of_analytics`,
	}
	before := map[string][][]string{}
	for k, q := range queries {
		before[k] = dump(t, pdb, q)
	}

	snap, err := Take(ctx, pdb, dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	for _, want := range []string{"of_analytics", "order_notes", "orders", "tags"} {
		found := false
		for _, got := range snap.Tables() {
			found = found || got == want
		}
		if !found {
			t.Fatalf("snapshot must cover %s, covers %v", want, snap.Tables())
		}
	}
	for _, got := range snap.Tables() {
		if strings.HasPrefix(got, "orders_fts") || got == "of_run_log" || got == "of_files" {
			t.Fatalf("snapshot must not cover %s", got)
		}
	}

	// The replay: add, change and delete rows (the delete cascades), bump counters,
	// log a page view, and touch files. Meanwhile the engine logs its own work.
	for _, q := range []string{
		`INSERT INTO orders (name, status) VALUES ('flat white test', 'queued')`,
		`UPDATE orders SET status = 'done', name = 'second mocha edited' WHERE id = 2`,
		`DELETE FROM orders WHERE id = 3`,
		`DELETE FROM orders WHERE id = 2`,
		`INSERT INTO tags (tag, uses) VALUES ('test', 9)`,
		`UPDATE tags SET uses = 99 WHERE tag = 'hot'`,
		`INSERT INTO of_analytics (page_path, user_agent) VALUES ('/staff', 'HeadlessChrome')`,
		`INSERT INTO of_run_log (run_id, kind, payload) VALUES ('run_x', 'message', '{}')`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, "uploads", "new"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "uploads", "new", "test.txt"), []byte("junk"), 0o600)
	_ = os.Remove(filepath.Join(dir, "uploads", "a.txt"))
	_ = os.Remove(filepath.Join(dir, "uploads", "b.txt"))
	_ = os.WriteFile(filepath.Join(dir, "uploads", "b.txt"), []byte("replaced"), 0o600)

	if err := snap.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for k, q := range queries {
		if got := dump(t, pdb, q); !reflect.DeepEqual(got, before[k]) {
			t.Errorf("%s not restored:\n got  %v\n want %v", k, got, before[k])
		}
	}
	if n := dump(t, pdb, `SELECT COUNT(*) FROM of_run_log WHERE run_id = 'run_x'`); n[0][0] != "1" {
		t.Errorf("engine tables must keep what was written during the replay, got %v", n)
	}
	for name, want := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
		b, err := os.ReadFile(filepath.Join(dir, "uploads", name))
		if err != nil || string(b) != want {
			t.Errorf("uploads/%s = %q, %v; want %q", name, b, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "uploads", "new")); !os.IsNotExist(err) {
		t.Errorf("files and folders the replay created must be gone: %v", err)
	}
	// The next real insert reuses the counter instead of leaving a gap.
	if _, err := pdb.ExecWrite(`INSERT INTO orders (name, status) VALUES ('real order', 'queued')`); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, pdb, `SELECT MAX(id) FROM orders`); got[0][0] != "4" {
		t.Errorf("next id = %v, want 4", got)
	}
}

func TestIsLocalDomain(t *testing.T) {
	for d, want := range map[string]bool{
		"":                         true,
		"localhost":                true,
		"my-app.localhost":         true,
		"127.0.0.1":                true,
		"shop.example.com":         false,
		"orders.mybusiness.coffee": false,
	} {
		if got := IsLocalDomain(d); got != want {
			t.Errorf("IsLocalDomain(%q) = %v, want %v", d, got, want)
		}
	}
}

// Only real browsers count as traffic: headless probes and flow sessions do not.
func TestRecentRealTraffic(t *testing.T) {
	pdb, _ := setupProject(t)
	// setupProject logged one real view "now".
	busy, err := RecentRealTraffic(pdb.Reader(), 10*time.Minute)
	if err != nil || !busy {
		t.Fatalf("a real view in the window must count: %v %v", busy, err)
	}
	if _, err := pdb.ExecWrite(`UPDATE of_analytics SET created_at = datetime('now', '-1 hour')`); err != nil {
		t.Fatal(err)
	}
	for _, ua := range []string{"Mozilla/5.0 HeadlessChrome/126.0", "Mozilla/5.0 (Linux; Android 14) Mobile " + FlowBrowserMarker} {
		if _, err := pdb.ExecWrite(`INSERT INTO of_analytics (page_path, user_agent) VALUES ('/', ?)`, ua); err != nil {
			t.Fatal(err)
		}
	}
	busy, err = RecentRealTraffic(pdb.Reader(), 10*time.Minute)
	if err != nil || busy {
		t.Fatalf("old views and test browsers must not count: %v %v", busy, err)
	}
}

// Around restores the data after the replay, and still restores it when the
// replay panics.
func TestAroundRestoresEvenWhenTheReplayPanics(t *testing.T) {
	pdb, dir := setupProject(t)
	ctx := WithOpenerFunc(context.Background(), func(ctx context.Context) (*Snapshot, string, error) {
		s, err := Take(ctx, pdb, dir)
		return s, "", err
	})
	count := func() string { return dump(t, pdb, `SELECT COUNT(*) FROM orders`)[0][0] }

	skip, err := Around(ctx, func() { _, _ = pdb.ExecWrite(`INSERT INTO orders (name) VALUES ('replay')`) })
	if skip != "" || err != nil || count() != "3" {
		t.Fatalf("skip=%q err=%v orders=%s, want the replay's row rolled back", skip, err, count())
	}

	func() {
		defer func() { _ = recover() }()
		_, _ = Around(ctx, func() {
			_, _ = pdb.ExecWrite(`INSERT INTO orders (name) VALUES ('replay that crashes')`)
			panic("boom")
		})
	}()
	if count() != "3" {
		t.Fatalf("orders=%s after a panicking replay, want 3", count())
	}

	if skip, _ := Around(context.Background(), func() { t.Fatal("must not replay without an opener") }); skip == "" {
		t.Fatal("no opener must mean a skip")
	}
}
