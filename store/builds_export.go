package store

import (
	"database/sql"
	"encoding/json"
)

// RunExists reports whether runID exists in of_runs.
func RunExists(reader *sql.DB, runID string) bool {
	var found string
	return reader.QueryRow(`SELECT id FROM of_runs WHERE id = ?`, runID).Scan(&found) == nil
}

// LoadRunExportStages loads stage rows from of_run_log for build export.
func LoadRunExportStages(reader *sql.DB, runID string) ([]map[string]any, error) {
	rows, err := reader.Query(
		`SELECT ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log WHERE run_id = ? AND kind = 'stage' ORDER BY id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stages := []map[string]any{}
	for rows.Next() {
		var ref, payload, createdAt string
		var inT, outT int
		var dur int64
		if err := rows.Scan(&ref, &payload, &inT, &outT, &dur, &createdAt); err != nil {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal([]byte(payload), &data)
		status, _ := data["status"].(string)
		errMsg, _ := data["error"].(string)
		stages = append(stages, map[string]any{
			"stage":         ref,
			"status":        status,
			"input_tokens":  inT,
			"output_tokens": outT,
			"duration_ms":   dur,
			"error_message": errMsg,
			"at":            createdAt,
		})
	}
	return stages, rows.Err()
}

// LoadRunExportEvents loads non-stage run-log events for build export.
func LoadRunExportEvents(reader *sql.DB, runID string) ([]map[string]any, error) {
	rows, err := reader.Query(
		`SELECT kind, ref, payload, created_at
		 FROM of_run_log WHERE run_id = ? AND kind != 'stage' ORDER BY id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []map[string]any{}
	for rows.Next() {
		var kind, ref, payload, createdAt string
		if err := rows.Scan(&kind, &ref, &payload, &createdAt); err != nil {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal([]byte(payload), &data)
		summary, _ := data["text"].(string)
		events = append(events, map[string]any{
			"event_type": kind,
			"summary":    summary,
			"details":    payload,
			"ref":        ref,
			"created_at": createdAt,
		})
	}
	return events, rows.Err()
}

// LoadRunChatMessages loads the brain chat transcript within a run's
// [startedAt, completedAt] window (empty/nil bound = unbounded on that side).
//
// Both bounds AND the stored created_at are wrapped in datetime() on purpose:
// the bounds read back RFC3339 (...T...Z) while created_at is written
// space-separated, and a raw string compare ('T' > ' ') silently empties the
// exported transcript. datetime() compares them as instants.
func LoadRunChatMessages(reader *sql.DB, startedAt string, completedAt *string) ([]map[string]any, error) {
	chatMessages := []map[string]any{}
	chatQuery := `SELECT role, COALESCE(content, ''), COALESCE(tool_calls, ''),
		 COALESCE(tool_call_id, ''), created_at
		 FROM of_chat_messages WHERE session_id = 'brain'`
	chatArgs := []any{}
	if startedAt != "" {
		chatQuery += ` AND datetime(created_at) >= datetime(?)`
		chatArgs = append(chatArgs, startedAt)
	}
	if completedAt != nil && *completedAt != "" {
		chatQuery += ` AND datetime(created_at) <= datetime(?)`
		chatArgs = append(chatArgs, *completedAt)
	}
	chatQuery += ` ORDER BY created_at ASC, id ASC`
	rows, err := reader.Query(chatQuery, chatArgs...)
	if err != nil {
		return chatMessages, err
	}
	defer rows.Close()

	for rows.Next() {
		var role, content, toolCalls, toolCallID, createdAt string
		if err := rows.Scan(&role, &content, &toolCalls, &toolCallID, &createdAt); err != nil {
			continue
		}
		chatMessages = append(chatMessages, map[string]any{
			"role":         role,
			"content":      content,
			"tool_calls":   toolCalls,
			"tool_call_id": toolCallID,
			"created_at":   createdAt,
		})
	}
	return chatMessages, rows.Err()
}
