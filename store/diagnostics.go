package store

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	"openfabrica/db"
)

// ProjectHealthCounts holds basic object counts for a project.
type ProjectHealthCounts struct {
	Pages    int `json:"pages"`
	Tables   int `json:"tables"`
	Memories int `json:"memories"`
}

// IntegrityIssue describes a structural warning or error in a project.
type IntegrityIssue struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ErrorLogEntry represents a failed stage entry from of_run_log.
type ErrorLogEntry struct {
	ID         int       `json:"id"`
	EventType  string    `json:"event_type"`
	Summary    string    `json:"summary"`
	Details    string    `json:"details"`
	TokensUsed int       `json:"tokens_used"`
	Model      string    `json:"model"`
	DurationMs int       `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

// LoadProjectHealthCounts returns page, dynamic-table, and memory counts.
func LoadProjectHealthCounts(pdb *db.ProjectDB) ProjectHealthCounts {
	var c ProjectHealthCounts
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0").Scan(&c.Pages); err != nil {
		slog.Debug("diagnostics: failed to count pages", "error", err)
	}
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables").Scan(&c.Tables); err != nil {
		slog.Debug("diagnostics: failed to count tables", "error", err)
	}
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_memory").Scan(&c.Memories); err != nil {
		slog.Debug("diagnostics: failed to count memories", "error", err)
	}
	return c
}

// CheckProjectIntegrity checks layout, page-layout references, and asset presence.
func CheckProjectIntegrity(pdb *db.ProjectDB) []IntegrityIssue {
	issues := []IntegrityIssue{}

	var currentStage string
	if err := pdb.QueryRow("SELECT stage FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1").Scan(&currentStage); err != nil && err != sql.ErrNoRows {
		slog.Debug("diagnostics: failed to fetch pipeline stage", "error", err)
	}
	buildComplete := currentStage == "live" || currentStage == ""

	var layoutCount int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'layout' AND is_deleted = 0").Scan(&layoutCount); err != nil {
		slog.Debug("diagnostics: failed to count layouts", "error", err)
	}
	if layoutCount == 0 {
		if buildComplete {
			issues = append(issues, IntegrityIssue{Level: "error", Message: "No layouts found. The project has no page structure."})
		} else {
			issues = append(issues, IntegrityIssue{Level: "info", Message: "No layouts yet, the engine is still building (stage: " + currentStage + ")"})
		}
	}

	if layoutCount > 0 {
		if rows, err := pdb.Query(
			`SELECT path, json_extract(metadata, '$.layout') FROM of_files
			 WHERE kind = 'page' AND is_deleted = 0
			   AND json_extract(metadata, '$.layout') IS NOT NULL
			   AND json_extract(metadata, '$.layout') != ''
			   AND json_extract(metadata, '$.layout') NOT IN (SELECT path FROM of_files WHERE kind = 'layout' AND is_deleted = 0)`,
		); err == nil {
			defer rows.Close()
			for rows.Next() {
				var path, layout string
				if rows.Scan(&path, &layout) == nil {
					issues = append(issues, IntegrityIssue{Level: "warning", Message: "Page '" + path + "' references missing layout '" + layout + "'"})
				}
			}
			_ = rows.Err()
		}
	}

	var assetCount int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'file' AND is_deleted = 0").Scan(&assetCount); err != nil {
		slog.Debug("diagnostics: failed to count assets", "error", err)
	}
	if assetCount == 0 {
		if buildComplete {
			issues = append(issues, IntegrityIssue{Level: "warning", Message: "No CSS/JS assets found. The project may have no styling."})
		} else {
			issues = append(issues, IntegrityIssue{Level: "info", Message: "No assets yet, the engine is still building (stage: " + currentStage + ")"})
		}
	}

	return issues
}

// ListStageErrorLogs returns recent failed stage rows from of_run_log.
func ListStageErrorLogs(pdb *db.ProjectDB, limit int) []ErrorLogEntry {
	rows, err := pdb.Query(
		`SELECT id, ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log
		 WHERE kind = 'stage' AND payload LIKE '%"status":"failed"%'
		 ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return []ErrorLogEntry{}
	}
	defer rows.Close()

	entries := []ErrorLogEntry{}
	for rows.Next() {
		var (
			id            int
			ref, payload  string
			inTok, outTok int
			durationMs    int
			createdAt     time.Time
		)
		if rows.Scan(&id, &ref, &payload, &inTok, &outTok, &durationMs, &createdAt) != nil {
			continue
		}
		entries = append(entries, ErrorLogEntry{
			ID:         id,
			EventType:  ref + " failed",
			Summary:    stageErrorMessage(payload),
			Details:    payload,
			TokensUsed: inTok + outTok,
			DurationMs: durationMs,
			CreatedAt:  createdAt,
		})
	}
	_ = rows.Err()
	return entries
}

func stageErrorMessage(payload string) string {
	var p struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &p) == nil && p.Error != "" {
		return p.Error
	}
	return payload
}
