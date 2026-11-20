package store

import (
	"database/sql"
	"time"

	"openfabrica/internal/dbh"
)

// FolderWatcher configures a local drop-zone folder monitored for new/updated files.
type FolderWatcher struct {
	ID           int
	Name         string
	Folder       string
	Glob         string
	FunctionPath string
	PollSec      int
	Enabled      bool
	CreatedAt    time.Time
}

// UpsertFolderWatcher creates or updates a local folder watcher definition.
func UpsertFolderWatcher(writer dbh.Handle, w FolderWatcher) error {
	if w.Glob == "" {
		w.Glob = "*"
	}
	if w.PollSec < 2 {
		w.PollSec = 5
	}
	if w.PollSec > 3600 {
		w.PollSec = 3600
	}
	_, err := writer.Exec(
		`INSERT INTO of_folder_watchers (name, folder, glob, function_path, poll_sec, enabled)
		 VALUES (?, ?, ?, ?, ?, 1)
		 ON CONFLICT(name) DO UPDATE SET
		   folder = excluded.folder,
		   glob = excluded.glob,
		   function_path = excluded.function_path,
		   poll_sec = excluded.poll_sec,
		   enabled = 1`,
		w.Name, w.Folder, w.Glob, w.FunctionPath, w.PollSec,
	)
	return err
}

// ListFolderWatchers lists all configured local folder watchers.
func ListFolderWatchers(reader dbh.Handle) ([]FolderWatcher, error) {
	rows, err := reader.Query(
		`SELECT id, name, folder, glob, function_path, poll_sec, enabled, created_at
		 FROM of_folder_watchers ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderWatcher
	for rows.Next() {
		var w FolderWatcher
		var enabled int
		if err := rows.Scan(&w.ID, &w.Name, &w.Folder, &w.Glob, &w.FunctionPath, &w.PollSec, &enabled, &w.CreatedAt); err != nil {
			return nil, err
		}
		w.Enabled = enabled != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteFolderWatcher deletes a watcher and its seen-file log by name.
func DeleteFolderWatcher(writer dbh.Handle, name string) (bool, error) {
	res, err := writer.Exec(`DELETE FROM of_folder_watchers WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		bestEffortExec(writer, "watch seen cleanup", `DELETE FROM of_folder_watch_seen WHERE watcher_name = ?`, name)
	}
	return n > 0, nil
}

// SeenWatchFile records the size and mtime of a previously processed file in a watched folder.
type SeenWatchFile struct {
	SizeBytes int64
	ModTimeNs int64
}

// ListWatchSeen loads all seen files for watcherName keyed by rel_path.
func ListWatchSeen(reader dbh.Handle, watcherName string) (map[string]SeenWatchFile, error) {
	rows, err := reader.Query(
		`SELECT rel_path, size_bytes, mod_time_ns FROM of_folder_watch_seen WHERE watcher_name = ?`,
		watcherName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]SeenWatchFile)
	for rows.Next() {
		var relPath string
		var sf SeenWatchFile
		if err := rows.Scan(&relPath, &sf.SizeBytes, &sf.ModTimeNs); err != nil {
			return nil, err
		}
		out[relPath] = sf
	}
	return out, rows.Err()
}

// DeleteWatchFileSeen removes a deleted file's seen record so a future drop with the same name is treated as created.
func DeleteWatchFileSeen(writer dbh.Handle, watcherName, relPath string) {
	bestEffortExec(writer, "watch delete seen",
		`DELETE FROM of_folder_watch_seen WHERE watcher_name = ? AND rel_path = ?`,
		watcherName, relPath,
	)
}

// IsWatchFileSeen reports whether (watcherName, relPath) was already processed
// at the exact same size and modTimeNs.
func IsWatchFileSeen(reader dbh.Handle, watcherName, relPath string, size, modTimeNs int64) bool {
	var seenSize, seenMod int64
	err := reader.QueryRow(
		`SELECT size_bytes, mod_time_ns FROM of_folder_watch_seen WHERE watcher_name = ? AND rel_path = ?`,
		watcherName, relPath,
	).Scan(&seenSize, &seenMod)
	if err == sql.ErrNoRows || err != nil {
		return false
	}
	return seenSize == size && seenMod == modTimeNs
}

// MarkWatchFileSeen records that a dropped file version has been processed.
func MarkWatchFileSeen(writer dbh.Handle, watcherName, relPath string, size, modTimeNs int64) {
	bestEffortExec(writer, "watch mark seen",
		`INSERT INTO of_folder_watch_seen (watcher_name, rel_path, size_bytes, mod_time_ns, processed_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(watcher_name, rel_path) DO UPDATE SET
		   size_bytes = excluded.size_bytes,
		   mod_time_ns = excluded.mod_time_ns,
		   processed_at = CURRENT_TIMESTAMP`,
		watcherName, relPath, size, modTimeNs,
	)
}
