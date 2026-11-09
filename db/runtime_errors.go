package db

import (
	"database/sql"
	"errors"
	"log/slog"

	"openfabrica/internal/dbh"
	"openfabrica/internal/stringutil"
)

// Runtime failures raised by sandboxed project code (of_runtime_errors).
//
// The public HTTP response is generic on purpose: a thrown message can carry table
// names, secret names, or internal paths, so it is never sent to the caller.
// Recording it here keeps the cause inside the project, where the endpoint probe
// and diagnostics tool can report it against the endpoint that raised it.

// RuntimeErrorFunction marks a failure raised by a manage_function endpoint.
const RuntimeErrorFunction = "function"

// runtimeErrorRing is how many rows are kept. Only the most recent failure per
// endpoint is ever read, so this is sized to keep a build's worth of history for
// a post-mortem, not to be a durable log.
const runtimeErrorRing = 50

// runtimeErrorMaxBytes clips one recorded message. A thrown exception carries a
// stack, and the readers quote it into a tool result.
const runtimeErrorMaxBytes = 2000

// RuntimeError is one recorded failure.
type RuntimeError struct {
	Source    string
	Path      string
	Message   string
	CreatedAt string
}

// RecordRuntimeError appends one failure and trims the ring back to its cap.
// Recording is best-effort: it runs on a request path that is already failing,
// so a write error is logged and swallowed rather than compounding the failure.
func RecordRuntimeError(h dbh.Handle, source, path, message string) {
	if h == nil || message == "" {
		return
	}
	if _, err := h.Exec(
		"INSERT INTO of_runtime_errors (source, path, message) VALUES (?, ?, ?)",
		source, path, stringutil.Truncate(message, runtimeErrorMaxBytes),
	); err != nil {
		slog.Debug("runtime error record failed", "source", source, "path", path, "error", err)
		return
	}
	// ids are AUTOINCREMENT and so strictly increasing: everything at or below
	// (newest - cap) is outside the window. Gaps only trim earlier, never more
	// than the cap, which is the bound this needs.
	if _, err := h.Exec(
		"DELETE FROM of_runtime_errors WHERE id <= (SELECT MAX(id) FROM of_runtime_errors) - ?",
		runtimeErrorRing,
	); err != nil {
		slog.Debug("runtime error trim failed", "error", err)
	}
}

// LastRuntimeError returns the most recent failure recorded for one source and
// path, and whether there was one. Callers use it to attach a cause to an
// otherwise opaque 5xx, so a miss is normal and not an error.
func LastRuntimeError(h dbh.Handle, source, path string) (RuntimeError, bool) {
	if h == nil {
		return RuntimeError{}, false
	}
	re := RuntimeError{Source: source, Path: path}
	err := h.QueryRow(
		"SELECT message, created_at FROM of_runtime_errors WHERE source = ? AND path = ? ORDER BY id DESC LIMIT 1",
		source, path,
	).Scan(&re.Message, &re.CreatedAt)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Debug("runtime error lookup failed", "source", source, "path", path, "error", err)
		}
		return RuntimeError{}, false
	}
	return re, true
}

// RecentRuntimeErrors returns the newest recorded failures across every source,
// most recent first.
func RecentRuntimeErrors(h dbh.Handle, limit int) ([]RuntimeError, error) {
	if h == nil || limit <= 0 {
		return nil, nil
	}
	rows, err := h.Query(
		"SELECT source, path, message, created_at FROM of_runtime_errors ORDER BY id DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuntimeError
	for rows.Next() {
		var re RuntimeError
		if err := rows.Scan(&re.Source, &re.Path, &re.Message, &re.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, rows.Err()
}
