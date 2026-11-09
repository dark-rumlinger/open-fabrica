package engine

import (
	"testing"
	"time"

	"openfabrica/internal/dbtest"
)

func TestPruneRetainedDropsOnlyOldFinishedRows(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	w := pdb.Writer()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	old := "datetime('now', '-200 days')"
	recent := "datetime('now', '-1 days')"

	exec(`INSERT INTO of_analytics (page_path, created_at) VALUES ('/old', ` + old + `), ('/new', ` + recent + `)`)
	exec(`INSERT INTO of_activity_log (event_type, created_at) VALUES ('old', ` + old + `), ('new', ` + recent + `)`)
	exec(`INSERT INTO of_jobs (type, status, created_at, completed_at) VALUES
		('a', 'completed', ` + old + `, ` + old + `),
		('b', 'pending', ` + old + `, NULL),
		('c', 'running', ` + old + `, NULL),
		('d', 'completed', ` + recent + `, ` + recent + `)`)
	exec(`INSERT INTO of_notifications (user_id, title, is_read, created_at) VALUES
		(1, 'old read', 1, ` + old + `),
		(1, 'old unread', 0, ` + old + `),
		(1, 'new read', 1, ` + recent + `)`)
	exec(`INSERT INTO of_processed_webhooks (event_key, created_at) VALUES ('stripe:old', ` + old + `), ('stripe:new', ` + recent + `)`)

	gone, err := pruneRetained(pdb, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{
		"of_analytics": 1, "of_activity_log": 1, "of_jobs": 1, "of_notifications": 1, "of_processed_webhooks": 1,
	} {
		if gone[table] != want {
			t.Errorf("%s: deleted %d, want %d", table, gone[table], want)
		}
	}

	var pendingOrRunning, unread int
	_ = w.QueryRow(`SELECT COUNT(*) FROM of_jobs WHERE status IN ('pending','running')`).Scan(&pendingOrRunning)
	_ = w.QueryRow(`SELECT COUNT(*) FROM of_notifications WHERE is_read = 0`).Scan(&unread)
	if pendingOrRunning != 2 {
		t.Errorf("pending/running jobs must never be pruned, %d left", pendingOrRunning)
	}
	if unread != 1 {
		t.Errorf("an unread notification must survive, %d left", unread)
	}
}

func TestRolloverForWindow(t *testing.T) {
	for _, c := range []struct {
		name            string
		trigger, window int
		want            int
	}{
		{"unknown window keeps the default", 110_000, 0, 110_000},
		{"a big window keeps the default", 110_000, 200_000, 110_000},
		{"a 32k local model rolls over early", 110_000, 32_768, 22_937},
		{"a window too small to build in keeps the default", 110_000, 8_192, 110_000},
		{"never raises the trigger", 20_000, 100_000, 20_000},
	} {
		if got := rolloverForWindow(c.trigger, c.window); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
