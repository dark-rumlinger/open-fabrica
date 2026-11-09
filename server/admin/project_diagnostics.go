package admin

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// ProjectDiagnosticsHandler handles health/integrity/errors for admin.
type ProjectDiagnosticsHandler struct {
	deps *Deps
}

// Health returns runtime and project-level stats.
func (h *ProjectDiagnosticsHandler) Health(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var pageCount, tableCount, memoryCount int
	if err := projectDB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0").Scan(&pageCount); err != nil {
		slog.Debug("diagnostics: failed to count pages", "error", err)
	}
	if err := projectDB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables").Scan(&tableCount); err != nil {
		slog.Debug("diagnostics: failed to count tables", "error", err)
	}
	if err := projectDB.QueryRow("SELECT COUNT(*) FROM of_memory").Scan(&memoryCount); err != nil {
		slog.Debug("diagnostics: failed to count memories", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"runtime": map[string]any{
			"go_version": runtime.Version(),
			"goroutines": runtime.NumGoroutine(),
			"cpus":       runtime.NumCPU(),
			"os":         runtime.GOOS,
			"arch":       runtime.GOARCH,
		},
		"memory": map[string]any{
			"alloc_mb":    mem.Alloc / 1024 / 1024,
			"total_alloc": mem.TotalAlloc / 1024 / 1024,
			"sys_mb":      mem.Sys / 1024 / 1024,
			"num_gc":      mem.NumGC,
		},
		"project": map[string]any{
			"pages":    pageCount,
			"tables":   tableCount,
			"memories": memoryCount,
		},
	})
}

type integrityIssue struct {
	Level   string `json:"level"` // "warning" or "error"
	Message string `json:"message"`
}

// Integrity checks layout, pages, and asset consistency.
func (h *ProjectDiagnosticsHandler) Integrity(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var issues []integrityIssue

	// Check current pipeline stage, suppress structural warnings during early build
	// stages where layouts/assets/memories haven't been created yet. The current
	// stage is the latest of_runs row's stage (lowercase engine codes).
	var currentStage string
	if err := projectDB.QueryRow("SELECT stage FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1").Scan(&currentStage); err != nil && err != sql.ErrNoRows {
		slog.Debug("diagnostics: failed to fetch pipeline stage", "error", err)
	}
	buildComplete := currentStage == "live" || currentStage == ""

	// Check layout exists
	var layoutCount int
	if err := projectDB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'layout' AND is_deleted = 0").Scan(&layoutCount); err != nil {
		slog.Debug("diagnostics: failed to count layouts", "error", err)
	}
	if layoutCount == 0 {
		if buildComplete {
			issues = append(issues, integrityIssue{"error", "No layouts found. The project has no page structure."})
		} else {
			issues = append(issues, integrityIssue{"info", "No layouts yet, the engine is still building (stage: " + currentStage + ")"})
		}
	}

	// Check pages reference valid layouts (only meaningful after layouts exist)
	if layoutCount > 0 {
		rows, err := projectDB.Query(
			`SELECT path, json_extract(metadata, '$.layout') FROM of_files
			 WHERE kind = 'page' AND is_deleted = 0
			   AND json_extract(metadata, '$.layout') IS NOT NULL
			   AND json_extract(metadata, '$.layout') != ''
			   AND json_extract(metadata, '$.layout') NOT IN (SELECT path FROM of_files WHERE kind = 'layout' AND is_deleted = 0)`,
		)
		if err == nil {
			issues = append(issues, collectRows(rows, func(rows *sql.Rows) (integrityIssue, error) {
				var path, layout string
				if err := rows.Scan(&path, &layout); err != nil {
					return integrityIssue{}, err
				}
				return integrityIssue{"warning", "Page '" + path + "' references missing layout '" + layout + "'"}, nil
			})...)
		}

	}

	// Check CSS/JS assets exist
	var assetCount int
	if err := projectDB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'file' AND is_deleted = 0").Scan(&assetCount); err != nil {
		slog.Debug("diagnostics: failed to count assets", "error", err)
	}
	if assetCount == 0 {
		if buildComplete {
			issues = append(issues, integrityIssue{"warning", "No CSS/JS assets found. The project may have no styling."})
		} else {
			issues = append(issues, integrityIssue{"info", "No assets yet, the engine is still building (stage: " + currentStage + ")"})
		}
	}

	if issues == nil {
		issues = []integrityIssue{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"issues": issues,
		"ok":     len(issues) == 0,
	})
}

type errorLogEntry struct {
	ID         int       `json:"id"`
	EventType  string    `json:"event_type"`
	Summary    string    `json:"summary"`
	Details    string    `json:"details"`
	TokensUsed int       `json:"tokens_used"`
	Model      string    `json:"model"`
	DurationMs int       `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

// Errors returns recent failure entries from of_run_log: a failed stage is a
// kind='stage' row whose payload JSON carries {"status":"failed","error":...}.
// The errorLogEntry wire shape carries no model field, so it stays empty.
func (h *ProjectDiagnosticsHandler) Errors(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	rows, err := projectDB.Query(
		`SELECT id, ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log
		 WHERE kind = 'stage' AND payload LIKE '%"status":"failed"%'
		 ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []errorLogEntry{})
		return
	}
	entries := collectRows(rows, func(rows *sql.Rows) (errorLogEntry, error) {
		var (
			id            int
			ref, payload  string
			inTok, outTok int
			durationMs    int
			createdAt     time.Time
		)
		if err := rows.Scan(&id, &ref, &payload, &inTok, &outTok, &durationMs, &createdAt); err != nil {
			return errorLogEntry{}, err
		}
		return errorLogEntry{
			ID:         id,
			EventType:  ref + " failed",
			Summary:    stageErrorMessage(payload),
			Details:    payload,
			TokensUsed: inTok + outTok,
			DurationMs: durationMs,
			CreatedAt:  createdAt,
		}, nil
	})

	if entries == nil {
		entries = []errorLogEntry{}
	}

	writeJSON(w, http.StatusOK, entries)
}

// stageErrorMessage extracts the error text from a kind='stage' run-log payload
// ({"status":...,"error":...}), falling back to the raw payload.
func stageErrorMessage(payload string) string {
	var p struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &p) == nil && p.Error != "" {
		return p.Error
	}
	return payload
}
