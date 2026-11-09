package chat

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"openfabrica/internal/buildstate"
	"openfabrica/providers/llm"
)

// LoadHistory reads the most recent messages for a session from the database
// and returns them as llm.Message values ordered oldest-first.
func LoadHistory(db *sql.DB, sessionID string, limit int) ([]llm.Message, error) {
	if limit <= 0 {
		limit = 50
	}

	// Select newest N rows, then reverse so we return oldest-first.
	rows, err := db.Query(`
		SELECT role, content, tool_calls, tool_call_id
		FROM of_chat_messages
		WHERE session_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("chat: load history: %w", err)
	}
	defer rows.Close()

	var msgs []llm.Message
	for rows.Next() {
		var (
			role       string
			content    string
			toolCalls  *string
			toolCallID *string
		)
		if err := rows.Scan(&role, &content, &toolCalls, &toolCallID); err != nil {
			return nil, fmt.Errorf("chat: scan message: %w", err)
		}

		msg := llm.Message{
			Role:    llm.Role(role),
			Content: content,
		}
		if toolCallID != nil {
			msg.ToolCallID = *toolCallID
		}
		if toolCalls != nil && *toolCalls != "" {
			var tcs []llm.ToolCall
			if err := json.Unmarshal([]byte(*toolCalls), &tcs); err == nil {
				msg.ToolCalls = tcs
			}
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat: iterate history: %w", err)
	}

	// Reverse so the oldest message comes first.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// MessageWithMeta wraps a message with metadata for merged history display.
// ID is the of_chat_messages PK. The client uses it to dedupe against live
// SSE-rendered messages on revisit (see the chat view's event hydration logic).
type MessageWithMeta struct {
	ID         int64          `json:"id"`
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	SessionID  string         `json:"session_id"`
	CreatedAt  string         `json:"created_at"`
}

// LoadMergedHistory reads the most recent messages from both the "admin" (user
// chat) and "brain" (engine) sessions for a project, merged by timestamp. Used
// for the chat UI display to show engine activity alongside user-initiated chat.
//
// Pagination uses the row id as the cursor, not created_at. created_at is a
// second-granularity CURRENT_TIMESTAMP, so a busy build can write dozens of rows
// in the same second; a `created_at < ?` cursor would skip every same-second row
// older than a page boundary (cut by LIMIT), silently dropping messages. id is a
// unique autoincrement key that grows monotonically with insertion, so ordering by
// id DESC matches created_at order while giving an exact, lossless cursor. beforeID
// <= 0 loads the newest page.
func LoadMergedHistory(db *sql.DB, limit int, beforeID int64) ([]MessageWithMeta, error) {
	if limit <= 0 {
		limit = 100
	}

	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = db.Query(`
			SELECT id, role, content, tool_calls, tool_call_id, session_id, created_at
			FROM of_chat_messages
			WHERE session_id IN ('admin', 'brain') AND id < ?
			ORDER BY id DESC
			LIMIT ?`,
			beforeID, limit,
		)
	} else {
		rows, err = db.Query(`
			SELECT id, role, content, tool_calls, tool_call_id, session_id, created_at
			FROM of_chat_messages
			WHERE session_id IN ('admin', 'brain')
			ORDER BY id DESC
			LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("chat: load merged history: %w", err)
	}
	defer rows.Close()

	var msgs []MessageWithMeta
	for rows.Next() {
		var (
			id         int64
			role       string
			content    string
			toolCalls  *string
			toolCallID *string
			sessionID  string
			createdAt  string
		)
		if err := rows.Scan(&id, &role, &content, &toolCalls, &toolCallID, &sessionID, &createdAt); err != nil {
			return nil, fmt.Errorf("chat: scan merged message: %w", err)
		}

		msg := MessageWithMeta{
			ID:        id,
			Role:      role,
			Content:   content,
			SessionID: sessionID,
			CreatedAt: createdAt,
		}
		if toolCallID != nil {
			msg.ToolCallID = *toolCallID
		}
		if toolCalls != nil && *toolCalls != "" {
			var tcs []llm.ToolCall
			if err := json.Unmarshal([]byte(*toolCalls), &tcs); err == nil {
				msg.ToolCalls = tcs
			}
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat: iterate merged history: %w", err)
	}

	// Reverse so the oldest message comes first.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// StageTransition is one completed pipeline stage, replayed from the engine
// run log (of_run_log) so the chat UI can show stage cards on history reload
// that match what the live event stream shows in real-time.
type StageTransition struct {
	Stage      string `json:"stage"`
	Status     string `json:"status"` // completed | failed
	DurationMS int64  `json:"duration_ms"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`
}

// PipelineSnapshot is the current state of the pipeline, used to seed the
// tracker on initial load. The frontend history snapshot is eventually
// consistent and may lag behind server state, so seed from authoritative
// pipeline state rather than reconstructing from message scanning.
type PipelineSnapshot struct {
	Stage       string `json:"stage"`
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason"`
	// CompletedStages seeds the tracker's completed nodes, independent of the
	// message window that bounds Stages above. Recurring stages (PLAN/BUILD) are
	// build-scoped to the latest build. EXPAND is project-scoped: it runs once on
	// the first build and its expanded_brief persists for the project's life, so
	// it is reported as completed whenever that brief exists, regardless of which
	// build logged it. Without this the optional leading EXPAND node would vanish
	// from the tracker on reload (and after change-request rebuilds).
	CompletedStages []string `json:"completed_stages,omitempty"`
}

// Timeline is the wrapped response for the initial admin chat load. It
// gives the renderer everything it needs to reconstruct the live view:
// messages, every completed stage transition, and the current pipeline cursor.
type Timeline struct {
	Messages []MessageWithMeta `json:"messages"`
	Stages   []StageTransition `json:"stages"`
	Pipeline *PipelineSnapshot `json:"pipeline,omitempty"`
}

// LoadTimeline returns the full chat reload payload: merged messages plus
// stage transitions plus a pipeline snapshot. Used by the admin chat's initial
// load. Pagination still goes through LoadMergedHistory directly.
//
// Stage transitions come from of_run_log (kind='stage', ref = stage name,
// payload JSON {status,error?}) scoped to the latest of_runs row, and the
// current cursor/pause comes from that latest run. Engine stage codes are
// lowercase; they are upper-cased here because the chat UI keys its tracker and
// stage cards on the uppercase names (EXPAND/PLAN/BUILD/LIVE).
func LoadTimeline(db *sql.DB, limit int) (*Timeline, error) {
	msgs, err := LoadMergedHistory(db, limit, 0)
	if err != nil {
		return nil, err
	}

	tl := &Timeline{Messages: msgs}

	// Bound stages to the time window of the loaded messages so transitions
	// from older builds don't sort above every visible message on reload.
	// Without this, the frontend's chronological merge piles every historical
	// stage-complete row at the top of the timeline.
	// Normalize both sides with datetime(): the sqlite driver scans a DATETIME
	// column (msgs[0].CreatedAt) back as RFC3339 (...T...Z) while of_run_log
	// stores the space-separated CURRENT_TIMESTAMP form, and a raw string
	// compare ('T' > ' ') would wrongly drop every in-window stage row.
	sinceClause := ""
	var stageArgs []any
	if len(msgs) > 0 {
		sinceClause = " AND datetime(created_at) >= datetime(?)"
		stageArgs = append(stageArgs, msgs[0].CreatedAt)
	}
	stageRows, err := db.Query(`
		SELECT ref, payload, COALESCE(duration_ms, 0), created_at
		FROM of_run_log
		WHERE kind = 'stage'`+sinceClause+`
		ORDER BY created_at ASC, id ASC`, stageArgs...)
	if err == nil {
		defer stageRows.Close()
		for stageRows.Next() {
			var ref, payload, createdAt string
			var durationMS int64
			if err := stageRows.Scan(&ref, &payload, &durationMS, &createdAt); err != nil {
				continue
			}
			status := stageStatusFromPayload(payload)
			if status != "completed" && status != "failed" {
				continue
			}
			tl.Stages = append(tl.Stages, StageTransition{
				Stage:      strings.ToUpper(ref),
				Status:     status,
				DurationMS: durationMS,
				// of_run_log rows carry a single timestamp (written at the
				// transition); duration_ms gives the elapsed time separately.
				StartedAt: createdAt,
				EndedAt:   createdAt,
			})
		}
		if err := stageRows.Err(); err != nil {
			slog.Warn("LoadTimeline: stage read failed", "error", err)
		}
	}

	// Current cursor + pause state: the latest run of any status.
	var runID, runStage, runStatus, runPause string
	if err := db.QueryRow(`
		SELECT id, stage, status, COALESCE(pause_reason, '')
		FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1`,
	).Scan(&runID, &runStage, &runStatus, &runPause); err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("LoadTimeline: run read failed", "error", err)
	}

	if runStage != "" {
		tl.Pipeline = &PipelineSnapshot{
			Stage:       strings.ToUpper(runStage),
			Paused:      runStatus == buildstate.RunPaused,
			PauseReason: runPause,
		}
		// Recurring stages (PLAN/BUILD) are build-scoped: report only those
		// completed in the latest run, unbounded by the message window.
		seen := map[string]bool{}
		if rows, err := db.Query(`
			SELECT ref, payload FROM of_run_log
			WHERE kind = 'stage' AND run_id = ?
			ORDER BY id ASC`, runID); err == nil {
			for rows.Next() {
				var ref, payload string
				if rows.Scan(&ref, &payload) == nil && stageStatusFromPayload(payload) == "completed" {
					up := strings.ToUpper(ref)
					if !seen[up] {
						seen[up] = true
						tl.Pipeline.CompletedStages = append(tl.Pipeline.CompletedStages, up)
					}
				}
			}
			if err := rows.Err(); err != nil {
				slog.Warn("LoadTimeline: run stage read failed", "error", err)
			}
			rows.Close()
		}
		// EXPAND is project-scoped, not build-scoped: it runs once on the first
		// build and its expanded_brief persists for the project's life (update
		// runs skip it). Surface it as completed whenever any run recorded a
		// brief, independent of which run logged it, so it stays on the tracker
		// across reloads and later change-request rebuilds.
		var expandedBrief string
		if err := db.QueryRow(`
			SELECT expanded_brief FROM of_runs
			WHERE expanded_brief != '' ORDER BY started_at DESC, id DESC LIMIT 1`,
		).Scan(&expandedBrief); err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("LoadTimeline: expanded_brief read failed", "error", err)
		}
		if expandedBrief != "" && !seen["EXPAND"] {
			tl.Pipeline.CompletedStages = append([]string{"EXPAND"}, tl.Pipeline.CompletedStages...)
		}
	}

	return tl, nil
}

// stageStatusFromPayload extracts the "status" field from a kind='stage'
// run-log payload (JSON {status,error?}); empty when absent or unparseable.
func stageStatusFromPayload(payload string) string {
	var p struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(payload), &p) != nil {
		return ""
	}
	return p.Status
}

// SaveMessage persists a single message to the of_chat_messages table.
func SaveMessage(db *sql.DB, sessionID string, msg llm.Message) error {
	var toolCallsJSON *string
	if len(msg.ToolCalls) > 0 {
		data, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return fmt.Errorf("chat: marshal tool_calls: %w", err)
		}
		s := string(data)
		toolCallsJSON = &s
	}

	var toolCallID *string
	if msg.ToolCallID != "" {
		toolCallID = &msg.ToolCallID
	}

	_, err := db.Exec(`
		INSERT INTO of_chat_messages (session_id, role, content, tool_calls, tool_call_id)
		VALUES (?, ?, ?, ?, ?)`,
		sessionID, string(msg.Role), msg.Content, toolCallsJSON, toolCallID,
	)
	if err != nil {
		return fmt.Errorf("chat: save message: %w", err)
	}
	return nil
}
