package admin

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/internal/backupsched"
	"openfabrica/internal/httpx"
)

// BackupsHandler serves the scheduled-backup settings and the files on disk. The
// schedule itself is run by internal/backupsched's loop (started in wiring); this
// handler reads/writes its settings, lists and downloads the files, and triggers
// a manual run. Admin-only (the archive holds the encryption and signing keys).
type BackupsHandler struct {
	deps *Deps
}

// backupTimeRe matches a 24-hour "HH:MM".
var backupTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func (h *BackupsHandler) cfg() backupsched.Config {
	return backupsched.Config{
		DataDir: h.deps.Config.DataDir,
		DBPath:  h.deps.Config.DBPath,
		Version: h.deps.Version,
	}
}

func (h *BackupsHandler) setting(key, def string) string {
	if v, _ := h.deps.DB.GetSetting(key); v != "" {
		return v
	}
	return def
}

func (h *BackupsHandler) retention() int {
	n, err := strconv.Atoi(h.setting(backupsched.SettingRetention, ""))
	if err != nil || n <= 0 {
		return backupsched.DefaultRetention
	}
	if n > 365 {
		return 365
	}
	return n
}

// Get returns the schedule settings, the backups on disk (newest first), and the
// next scheduled run.
//
//	GET /admin/api/system/backups
func (h *BackupsHandler) Get(w http.ResponseWriter, r *http.Request) {
	schedule := h.setting(backupsched.SettingSchedule, "off")
	tm := h.setting(backupsched.SettingTime, backupsched.DefaultTime)
	weekday := h.setting(backupsched.SettingWeekday, "0")

	items, err := backupsched.List(h.deps.Config.DataDir)
	if err != nil {
		h.deps.serverError(w, "failed to list backups", err)
		return
	}

	resp := map[string]any{
		"schedule":  schedule,
		"time":      tm,
		"weekday":   weekday,
		"retention": h.retention(),
		"dir":       h.cfg().Dir(),
		"items":     items,
	}
	if schedule == "daily" || schedule == "weekly" {
		if nr := backupsched.NextRun(schedule, tm, weekday, time.Now()); !nr.IsZero() {
			resp["next_run"] = nr
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// Update saves the schedule settings. Fields are validated here even though the
// UI constrains them, so the stored values the scheduler reads are always sane.
//
//	PUT /admin/api/system/backups
func (h *BackupsHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule  string `json:"schedule"`
		Time      string `json:"time"`
		Weekday   string `json:"weekday"`
		Retention int    `json:"retention"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	switch req.Schedule {
	case "off", "daily", "weekly":
	default:
		writeError(w, http.StatusBadRequest, "invalid backup settings")
		return
	}
	if req.Time != "" && !backupTimeRe.MatchString(req.Time) {
		writeError(w, http.StatusBadRequest, "invalid backup settings")
		return
	}
	if req.Weekday != "" {
		if n, err := strconv.Atoi(req.Weekday); err != nil || n < 0 || n > 6 {
			writeError(w, http.StatusBadRequest, "invalid backup settings")
			return
		}
	}
	if req.Retention < 1 || req.Retention > 365 {
		writeError(w, http.StatusBadRequest, "invalid backup settings")
		return
	}

	tm := req.Time
	if tm == "" {
		tm = backupsched.DefaultTime
	}
	wd := req.Weekday
	if wd == "" {
		wd = "0"
	}
	values := map[string]string{
		backupsched.SettingSchedule:  req.Schedule,
		backupsched.SettingTime:      tm,
		backupsched.SettingWeekday:   wd,
		backupsched.SettingRetention: strconv.Itoa(req.Retention),
	}
	for key, val := range values {
		if err := h.deps.DB.SetSetting(key, val); err != nil {
			h.deps.serverError(w, "failed to save settings", err)
			return
		}
	}
	writeStatus(w, "saved")
}

// Run creates one backup now and returns its info. Shares a lock with the
// scheduler, so it never overlaps a scheduled run.
//
//	POST /admin/api/system/backups/run
func (h *BackupsHandler) Run(w http.ResponseWriter, r *http.Request) {
	// A large instance can outlive the blanket 120 s write timeout before the
	// JSON reply is written.
	httpx.ExtendIO(w, 30*time.Minute)
	info, err := backupsched.RunOnce(h.cfg(), h.retention(), h.deps.Logger)
	if err != nil {
		h.deps.serverError(w, "failed to create backup", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// Download streams one backup file. The name is matched against the strict
// backup-filename grammar first, so it can never escape the backups directory.
//
//	GET /admin/api/system/backups/{name}
func (h *BackupsHandler) Download(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !backupsched.ValidName(name) {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	f, err := os.Open(filepath.Join(h.cfg().Dir(), name))
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	defer f.Close()

	httpx.ExtendIO(w, 30*time.Minute)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

// Delete removes one backup file.
//
//	DELETE /admin/api/system/backups/{name}
func (h *BackupsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !backupsched.ValidName(name) {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if err := os.Remove(filepath.Join(h.cfg().Dir(), name)); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "backup not found")
			return
		}
		h.deps.serverError(w, "failed to delete backup", err)
		return
	}
	writeStatus(w, "deleted")
}
