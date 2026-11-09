package backupsched

import (
	"path/filepath"
	"testing"
	"time"

	"openfabrica/db"

	_ "modernc.org/sqlite"
)

// Schedule math is asserted by relationship (fire <= now < next, within one
// period, right weekday) rather than hardcoded dates, so it holds whatever day
// the test runs. A one-hour slack absorbs any DST wall-clock wobble.
func TestDailySchedule(t *testing.T) {
	now := time.Now()
	mr := mostRecentFire("daily", "03:00", "", now)
	if mr.After(now) || now.Sub(mr) > 24*time.Hour+time.Hour {
		t.Errorf("daily most-recent out of range: %v (now %v)", mr, now)
	}
	if mr.Hour() != 3 || mr.Minute() != 0 {
		t.Errorf("daily most-recent wrong time: %v", mr)
	}
	nx := NextRun("daily", "03:00", "", now)
	if !nx.After(now) || nx.Sub(now) > 24*time.Hour+time.Hour {
		t.Errorf("daily next out of range: %v (now %v)", nx, now)
	}
}

func TestWeeklySchedule(t *testing.T) {
	now := time.Now()
	mr := mostRecentFire("weekly", "03:00", "3", now) // Wednesday
	if mr.After(now) || now.Sub(mr) > 7*24*time.Hour+time.Hour {
		t.Errorf("weekly most-recent out of range: %v (now %v)", mr, now)
	}
	if int(mr.Weekday()) != 3 {
		t.Errorf("weekly most-recent weekday = %v, want Wednesday", mr.Weekday())
	}
	nx := NextRun("weekly", "03:00", "3", now)
	if !nx.After(now) || nx.Sub(now) > 7*24*time.Hour+time.Hour {
		t.Errorf("weekly next out of range: %v (now %v)", nx, now)
	}
	if int(nx.Weekday()) != 3 {
		t.Errorf("weekly next weekday = %v, want Wednesday", nx.Weekday())
	}
}

func TestScheduleOffAndInvalid(t *testing.T) {
	now := time.Now()
	if !mostRecentFire("off", "03:00", "", now).IsZero() {
		t.Error("off schedule should never fire")
	}
	if !mostRecentFire("daily", "25:61", "", now).IsZero() {
		t.Error("invalid time should never fire")
	}
	if !NextRun("off", "03:00", "", now).IsZero() {
		t.Error("off schedule should have no next run")
	}
}

// RunOnce writes a real archive, and retention keeps only the newest N.
func TestRunOnceListPrune(t *testing.T) {
	dir := t.TempDir()
	// A valid core DB so the VACUUM INTO snapshot has something to copy.
	database, err := db.Open(filepath.Join(dir, "openfabrica.db"))
	if err != nil {
		t.Fatalf("open core db: %v", err)
	}
	_ = database.Close()

	cfg := Config{DataDir: dir, DBPath: filepath.Join(dir, "openfabrica.db"), Version: "test"}

	first, err := RunOnce(cfg, 1, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !ValidName(first.Name) || first.Size == 0 {
		t.Fatalf("unexpected first backup: %+v", first)
	}
	// Distinct second so the timestamped filename differs.
	time.Sleep(1100 * time.Millisecond)
	second, err := RunOnce(cfg, 1, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	items, err := List(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("retention keep=1 should leave 1 backup, got %d", len(items))
	}
	if items[0].Name != second.Name {
		t.Errorf("kept %q, want the newest %q", items[0].Name, second.Name)
	}
}
