package tools

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"openfabrica/events"
	"openfabrica/internal/dbh"
)

const approvalKeyPrefix = "approval:"

// CodeApprovalRequired is the stable Result.Code that RequestApproval sets so
// the engine session loop recognizes an approval pause and ends the segment
// cleanly (see engine/session_exec.go). The matching run row is paused with
// pause_reason 'awaiting_approval' so the worker stays parked until the owner
// answers and a wake resumes it.
const CodeApprovalRequired = "approval_required"

// latestApprovalAnswer returns the most recent owner answer for an approval key,
// lower-cased and trimmed. The bool is false when no answer exists yet.
func latestApprovalAnswer(db dbh.Handle, actionKey string) (string, bool) {
	var answer string
	err := db.QueryRow(`
		SELECT a.answer FROM of_questions q
		JOIN of_answers a ON a.question_id = q.id
		WHERE q.context = ?
		ORDER BY a.created_at DESC LIMIT 1
	`, approvalKeyPrefix+actionKey).Scan(&answer)
	if err != nil {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(answer)), true
}

// firstAnswerWord returns the first whitespace-delimited token of an owner
// answer (already lower-cased/trimmed by latestApprovalAnswer), stripped of
// trailing punctuation, so "no, thanks" and "yes." classify like their bare
// forms. Word-boundary matching - never a raw prefix over the whole answer -
// keeps "not sure" from reading as "no".
func firstAnswerWord(answer string) string {
	fields := strings.Fields(answer)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimRight(fields[0], ".,!;:")
}

// CheckApproval checks if an owner approval exists for the given action key.
// Returns true only if the owner explicitly approved.
func CheckApproval(db dbh.Handle, actionKey string) bool {
	answer, ok := latestApprovalAnswer(db, actionKey)
	if !ok {
		return false
	}
	w := firstAnswerWord(answer)
	return strings.HasPrefix(w, "yes") || strings.HasPrefix(w, "approve")
}

// isApprovalDenied checks if the owner explicitly denied an action. Matching
// is symmetric with CheckApproval (first word, not the whole answer) so a
// casual "no thanks" or "nope" counts as a denial instead of leaving the run
// parked in neither state.
func isApprovalDenied(db dbh.Handle, actionKey string) bool {
	answer, ok := latestApprovalAnswer(db, actionKey)
	if !ok {
		return false
	}
	switch firstAnswerWord(answer) {
	case "no", "nope", "deny", "denied", "reject", "rejected", "cancel", "canceled", "cancelled":
		return true
	}
	return false
}

// RequestApproval creates a high-urgency approval question, pauses the active
// run so the engine waits, and returns an error Result tagged with
// CodeApprovalRequired (which the session loop turns into a clean segment
// pause). When the owner answers via the admin UI the EventQuestionAnswered
// handler wakes the worker, which resumes the run and re-runs the current
// stage. On retry the tool calls CheckApproval again and proceeds if approved.
//
// If the owner already denied the action, a denial result is returned
// without pausing.
func RequestApproval(ctx *ToolContext, actionKey, question, detail string) *Result {
	// Already denied, don't pause, just tell the LLM.
	if isApprovalDenied(ctx.DB, actionKey) {
		return Fail("ACTION_DENIED: The owner denied this action. %s", detail)
	}

	key := approvalKeyPrefix + actionKey

	// Check if a pending question for this key already exists.
	var existingID int
	err := ctx.DB.QueryRow(
		"SELECT id FROM of_questions WHERE context = ? AND status = 'pending'", key,
	).Scan(&existingID)

	if err == sql.ErrNoRows {
		// Create the approval question.
		result, insErr := ctx.DB.Exec(
			`INSERT INTO of_questions (question, context, options, urgency, status, type)
			 VALUES (?, ?, 'Approve, Deny', 'high', 'pending', 'approval')`,
			question, key,
		)
		if insErr == nil {
			id, _ := result.LastInsertId()
			if ctx.Bus != nil {
				ctx.Bus.Publish(events.NewEvent(events.EventQuestionAsked, ctx.ProjectID, map[string]any{
					"id":       id,
					"question": question,
					"options":  "Approve, Deny",
					"urgency":  "high",
					"type":     "approval",
				}))
			}
		}
	}

	// Pause the active run so the engine waits for the answer. An in-run pause
	// lives on the of_runs row (status='paused', pause_reason set), exactly like
	// engine.Run.pause. Only the single running run is ever a match.
	if _, err := ctx.DB.Exec(
		"UPDATE of_runs SET status = 'paused', pause_reason = 'awaiting_approval', updated_at = CURRENT_TIMESTAMP WHERE status = 'running'",
	); err != nil {
		slog.Error("failed to pause run for approval", "action", actionKey, "error", err)
		return Fail("failed to pause run: %v", err)
	}

	return FailCode(CodeApprovalRequired,
		"APPROVAL_REQUIRED: %s This action requires owner approval. A question has been created in the admin panel. The build will resume automatically when the owner responds.",
		detail,
	)
}

// AuditExecutor is the QueryRow + Exec surface LogDestructiveAction needs.
// Both *sql.DB and *sql.Tx satisfy it, so a destructive action can record its
// audit row on the request DB or compose into an open transaction. Passing the
// active transaction is required when the caller already holds one: the writer
// pool is a single connection, so a second query on ctx.DB while a transaction
// is open would block waiting for the connection the transaction owns.
type AuditExecutor interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// LogDestructiveAction records a destructive tool action in the of_run_log
// journal for audit trail purposes. Called for Tier 2 actions (delete rows,
// delete files, delete endpoints, etc.) that don't require approval but
// should be tracked. A destructive action is a kind='tool' row in of_run_log
// attributed to the current run (run_id empty when none is in flight, e.g. a
// live edit). exec is the request DB for standalone calls, or the active
// transaction when the action runs inside one.
func LogDestructiveAction(exec AuditExecutor, tool, action, target string) {
	summary := fmt.Sprintf("Destructive: %s -> %s on %q", tool, action, target)
	var runID string
	_ = exec.QueryRow("SELECT id FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1").Scan(&runID)
	payload, _ := json.Marshal(map[string]any{
		"action":      action,
		"target":      target,
		"destructive": true,
		"ok":          true,
		"summary":     summary,
	})
	if _, err := exec.Exec(
		"INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES (?, 'tool', ?, ?)",
		runID, tool, string(payload),
	); err != nil {
		slog.Warn("failed to log destructive action", "summary", summary, "error", err)
	}
}
