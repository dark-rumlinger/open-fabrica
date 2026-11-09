// Package backupsched runs whole-instance backups on a schedule and manages the
// backup files on disk. It reuses internal/backup (VACUUM INTO snapshots, safe on
// a running server) to write a zip into <DataDir>/backups, which internal/backup
// already excludes from future archives, so backups never nest.
//
// The scheduler reads its configuration from the core settings table on every
// tick, so an edit from the admin UI applies within a minute without a restart.
// A manual "back up now" from the admin handler shares the same package mutex, so
// a manual and a scheduled run never overlap.
package backupsched

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"openfabrica/db"
	"openfabrica/internal/backup"
)

// Settings keys in the core settings table. Schedule/Time/Weekday/Retention are
// owner-facing; lastRun is internal bookkeeping so a restart neither double-runs a
// slot nor silently skips one it was down for.
const (
	SettingSchedule  = "backup_schedule"  // "off" | "daily" | "weekly"
	SettingTime      = "backup_time"      // "HH:MM", local time
	SettingWeekday   = "backup_weekday"   // "0".."6", Sunday = 0 (weekly only)
	SettingRetention = "backup_retention" // integer, newest N kept
	settingLastRun   = "backup_last_run"  // RFC3339 of the last scheduled attempt
)

const (
	// DefaultTime is the fallback backup time when none is set.
	DefaultTime = "03:00"
	// DefaultRetention is how many backups are kept when retention is unset.
	DefaultRetention = 7
	maxRetention     = 365
)

// nameRe matches a backup filename exactly, so a user-supplied name can never
// escape the backups directory (path traversal) on download or delete.
var nameRe = regexp.MustCompile(`^backup-\d{8}-\d{6}\.zip$`)

// runMu serializes every backup write (scheduled or manual) so two never run at
// once against the same files.
var runMu sync.Mutex

// Config locates the instance on disk for a backup run.
type Config struct {
	DataDir string
	DBPath  string
	Version string
}

// Dir is the backups directory, <DataDir>/backups.
func (c Config) Dir() string { return filepath.Join(c.DataDir, "backups") }

// Info describes one backup file on disk.
type Info struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidName reports whether name is a well-formed backup filename. Callers that
// take a name from a request must check this before joining it to a path.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// RunOnce writes one backup into the backups directory and prunes to the `keep`
// newest. It is serialized with the scheduler. The archive is written to a .tmp
// file and renamed into place, so a partial zip is never visible as a backup.
func RunOnce(cfg Config, keep int, log *slog.Logger) (Info, error) {
	runMu.Lock()
	defer runMu.Unlock()

	dir := cfg.Dir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Info{}, err
	}
	name := "backup-" + time.Now().Format("20060102-150405") + ".zip"
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return Info{}, err
	}
	_, createErr := backup.Create(f, backup.Options{
		DataDir: cfg.DataDir,
		DBPath:  cfg.DBPath,
		Version: cfg.Version,
		Skip:    tmp,
	})
	closeErr := f.Close()
	if createErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if createErr != nil {
			return Info{}, createErr
		}
		return Info{}, closeErr
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return Info{}, err
	}
	if keep > 0 {
		prune(dir, keep, log)
	}

	var size int64
	if st, err := os.Stat(final); err == nil {
		size = st.Size()
	}
	if log != nil {
		log.Info("backup created", "name", name, "size", size)
	}
	return Info{Name: name, Size: size, CreatedAt: time.Now()}, nil
}

// List returns the backups under <dataDir>/backups, newest first. A missing
// directory is not an error (no backups yet).
func List(dataDir string) ([]Info, error) {
	return listDir(filepath.Join(dataDir, "backups"))
}

func listDir(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Info{}, nil
		}
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !nameRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: e.Name(), Size: fi.Size(), CreatedAt: fi.ModTime()})
	}
	// The name is a timestamp, so a reverse name sort is newest-first.
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

func prune(dir string, keep int, log *slog.Logger) {
	infos, err := listDir(dir)
	if err != nil {
		return
	}
	for i := keep; i < len(infos); i++ {
		if err := os.Remove(filepath.Join(dir, infos[i].Name)); err != nil && log != nil {
			log.Warn("backup prune failed", "name", infos[i].Name, "error", err)
		}
	}
}

// parseHM parses "HH:MM" in 24-hour time.
func parseHM(s string) (h, m int, ok bool) {
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func weekday(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 6 {
		return 0
	}
	return n
}

// mostRecentFire returns the latest scheduled instant at or before now, or the
// zero time when the schedule or time is invalid.
func mostRecentFire(schedule, hm, weekdayStr string, now time.Time) time.Time {
	h, m, ok := parseHM(hm)
	if !ok {
		return time.Time{}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	switch schedule {
	case "daily":
		if today.After(now) {
			return today.AddDate(0, 0, -1)
		}
		return today
	case "weekly":
		delta := (int(now.Weekday()) - weekday(weekdayStr) + 7) % 7
		cand := today.AddDate(0, 0, -delta)
		if cand.After(now) {
			cand = cand.AddDate(0, 0, -7)
		}
		return cand
	}
	return time.Time{}
}

// NextRun returns the next scheduled instant strictly after now, for display. The
// zero time means "not scheduled".
func NextRun(schedule, hm, weekdayStr string, now time.Time) time.Time {
	h, m, ok := parseHM(hm)
	if !ok {
		return time.Time{}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	switch schedule {
	case "daily":
		if today.After(now) {
			return today
		}
		return today.AddDate(0, 0, 1)
	case "weekly":
		delta := (weekday(weekdayStr) - int(now.Weekday()) + 7) % 7
		cand := today.AddDate(0, 0, delta)
		if !cand.After(now) {
			cand = cand.AddDate(0, 0, 7)
		}
		return cand
	}
	return time.Time{}
}

// Scheduler runs backups on the configured cadence until its context is done.
type Scheduler struct {
	cfg Config
	db  *db.DB
	log *slog.Logger
}

// New builds a scheduler. It does not start until Run is called.
func New(database *db.DB, cfg Config, log *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, db: database, log: log}
}

// Run checks once a minute whether a scheduled backup is due and runs it. It
// returns when ctx is cancelled (process shutdown).
func (s *Scheduler) Run(ctx context.Context) {
	tk := time.NewTicker(time.Minute)
	defer tk.Stop()
	s.tick(time.Now()) // catch up a slot missed while the server was down
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			s.tick(now)
		}
	}
}

func (s *Scheduler) tick(now time.Time) {
	schedule := s.get(SettingSchedule)
	if schedule != "daily" && schedule != "weekly" {
		return
	}
	fire := mostRecentFire(schedule, s.getOr(SettingTime, DefaultTime), s.get(SettingWeekday), now)
	if fire.IsZero() {
		return
	}
	// One run per slot: skip when the last attempt already covered this fire time.
	if last := s.lastRun(); !last.Before(fire) {
		return
	}

	info, err := RunOnce(s.cfg, s.retention(), s.log)
	// Advance the marker whether or not the run succeeded, so a failing backup is
	// retried at the next scheduled slot rather than every minute. The failure is
	// logged and audited.
	_ = s.db.SetSetting(settingLastRun, now.Format(time.RFC3339))
	status := 200
	if err != nil {
		status = 500
		if s.log != nil {
			s.log.Error("scheduled backup failed", "error", err)
		}
	}
	s.audit(status, info.Name)
}

func (s *Scheduler) get(key string) string {
	v, _ := s.db.GetSetting(key)
	return v
}

func (s *Scheduler) getOr(key, def string) string {
	if v := s.get(key); v != "" {
		return v
	}
	return def
}

func (s *Scheduler) retention() int {
	n, err := strconv.Atoi(s.get(SettingRetention))
	if err != nil || n <= 0 {
		return DefaultRetention
	}
	if n > maxRetention {
		return maxRetention
	}
	return n
}

func (s *Scheduler) lastRun() time.Time {
	t, _ := time.Parse(time.RFC3339, s.get(settingLastRun))
	return t
}

// audit records a scheduled backup in the admin audit trail as the "system"
// actor, so it appears in the Activity view alongside operator actions.
func (s *Scheduler) audit(status int, name string) {
	_, _ = s.db.ExecWrite(
		`INSERT INTO admin_audit (user_id, username, method, path, status) VALUES (0, 'system', 'BACKUP', ?, ?)`,
		"/system/backups/"+name, status,
	)
}
