package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"openfabrica/events"
)

// --- Multi-field form questions ---

// formField is one field definition of a form question (of_questions.fields).
type formField struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Type       string `json:"type"`
	SecretName string `json:"secret_name"`
}

func (f formField) displayLabel() string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

// parseFormFields decodes the fields JSON, keeping only usable (named) fields.
func parseFormFields(fieldsJSON string) []formField {
	if fieldsJSON == "" || fieldsJSON == "[]" || fieldsJSON == "null" {
		return nil
	}
	var fields []formField
	if json.Unmarshal([]byte(fieldsJSON), &fields) != nil {
		return nil
	}
	out := fields[:0]
	for _, f := range fields {
		if f.Name != "" {
			out = append(out, f)
		}
	}
	return out
}

// startFormFlow arms the sequential form flow and prompts the first field. Each
// field is answered one at a time; values are stored as they arrive (secrets
// encrypted, others into of_memory), matching the admin's form handling.
// Caller must hold state.Mu.
func (s *BotService) startFormFlow(state *UserState, questionID, question, fieldsJSON string) {
	v := s.voice(state)
	fields := parseFormFields(fieldsJSON)
	if len(fields) == 0 {
		// No usable fields: fall back to free text so the flow still resumes.
		state.PendingAction = "awaiting_answer"
		state.PendingActionAt = time.Now()
		state.PendingData = map[string]string{"question_id": questionID}
		SaveSession(s.deps.DB, state)
		s.send(state, v.trh("Question: %s", question)+"\n\n"+v.tr("Reply with your answer:"))
		return
	}

	state.PendingAction = "awaiting_form"
	state.PendingActionAt = time.Now()
	state.PendingData = map[string]string{
		"question_id":     questionID,
		"form_index":      "0",
		"form_configured": "",
	}
	SaveSession(s.deps.DB, state)

	intro := v.trh("📝 %s", question) + "\n\n" + v.tr("I'll ask for each field, one at a time (%d in total). Send /cancel to stop.", len(fields))
	s.send(state, intro)
	s.promptFormField(state, fields, 0)
}

// promptFormField asks for a single field's value.
func (s *BotService) promptFormField(state *UserState, fields []formField, idx int) {
	v := s.voice(state)
	fd := fields[idx]
	if fd.Type == "secret" {
		s.send(state, v.trh("Field %d/%d - 🔐 <b>%s</b>\nReply with the value; I'll store it encrypted and delete your message.", idx+1, len(fields), fd.displayLabel()))
		return
	}
	s.send(state, v.trh("Field %d/%d - <b>%s</b>\nReply with the value.", idx+1, len(fields), fd.displayLabel()))
}

// handleFormReply records one field's value and advances the form, finalizing
// the question once every field is answered. Caller must hold state.Mu.
func (s *BotService) handleFormReply(state *UserState, text string, msgID int) {
	questionID := state.PendingData["question_id"]
	idx := 0
	_, _ = fmt.Sscanf(state.PendingData["form_index"], "%d", &idx)

	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		s.clearPending(state)
		s.send(state, s.voice(state).tr("Project database unavailable. Please try again later."))
		return
	}

	// Re-read the field defs and status so a re-plan (superseded) or a web-UI
	// answer aborts the flow cleanly.
	var fieldsJSON, status string
	if err := pdb.QueryRow("SELECT COALESCE(fields,''), COALESCE(status,'pending') FROM of_questions WHERE id = ?", questionID).Scan(&fieldsJSON, &status); err != nil || status != "pending" {
		s.clearPending(state)
		if err != nil {
			s.handleChat(state, text) // question gone; treat as ordinary chat
		}
		return
	}
	fields := parseFormFields(fieldsJSON)
	if idx >= len(fields) {
		s.clearPending(state)
		return
	}

	value := strings.TrimSpace(text)
	if value == "" {
		s.send(state, s.voice(state).tr("Please send a value for this field (or /cancel to stop)."))
		return // stay on the same field
	}

	s.storeFormField(state, questionID, fields[idx], value, msgID)

	configured := state.PendingData["form_configured"]
	if configured != "" {
		configured += ", "
	}
	configured += fields[idx].displayLabel()

	idx++
	if idx < len(fields) {
		state.PendingData["form_index"] = strconv.Itoa(idx)
		state.PendingData["form_configured"] = configured
		SaveSession(s.deps.DB, state)
		s.promptFormField(state, fields, idx)
		return
	}

	// All fields collected: finalize.
	s.clearPending(state)
	remaining, ok := s.finalizeFormAnswer(state, questionID, "Configured: "+configured)
	if !ok {
		return
	}
	s.afterAnswer(state, remaining)
}

// storeFormField persists one field value: secrets are encrypted into of_secrets
// (and the pasted message scrubbed), everything else upserts into of_memory,
// identical to the admin's processFieldAnswer.
func (s *BotService) storeFormField(state *UserState, questionID string, fd formField, value string, userMsgID int) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return
	}
	if fd.Type == "secret" && fd.SecretName != "" {
		enc, err := s.deps.Encryptor.Encrypt(value)
		if err != nil {
			s.logger.Warn("storeFormField: encrypt failed", "field", fd.Name, "error", err)
			return
		}
		if _, err := pdb.ExecWrite(
			`INSERT INTO of_secrets (name, value_encrypted, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
			fd.SecretName, enc); err != nil {
			s.logger.Warn("storeFormField: store secret failed", "field", fd.Name, "error", err)
			return
		}
		if userMsgID > 0 {
			if p, ok := s.provider(state.Provider); ok {
				_ = p.DeleteMessage(state.ChatID, userMsgID)
			}
		}
		var qid any
		if id, err := strconv.Atoi(questionID); err == nil {
			qid = id
		}
		s.deps.Bus.Publish(events.NewEvent(events.EventSecretStored, state.ActiveProjectID, map[string]any{
			"name":        fd.SecretName,
			"question_id": qid,
		}))
		return
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_memory (key, value, category) VALUES (?, ?, 'field_answer')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		fd.Name, value); err != nil {
		s.logger.Warn("storeFormField: store memory failed", "field", fd.Name, "error", err)
	}
}

// finalizeFormAnswer marks the form question answered and records the summary,
// idempotently (a redelivery affects 0 rows). Returns remaining pending count.
func (s *BotService) finalizeFormAnswer(state *UserState, questionID, summary string) (int, bool) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return -1, false
	}

	var answeredBy any
	if state.UserID != "" {
		if id, err := strconv.ParseInt(state.UserID, 10, 64); err == nil {
			answeredBy = id
		}
	}

	var affected int64
	txErr := pdb.WithTx(func(tx *sql.Tx) error {
		res, e := tx.Exec("UPDATE of_questions SET status='answered', updated_at=datetime('now') WHERE id = ? AND status='pending'", questionID)
		if e != nil {
			return e
		}
		affected, _ = res.RowsAffected()
		if affected == 0 {
			return nil
		}
		_, e = tx.Exec("INSERT INTO of_answers (question_id, answer, answered_by) VALUES (?, ?, ?)", questionID, summary, answeredBy)
		return e
	})
	if txErr != nil {
		s.logger.Warn("finalizeFormAnswer: tx failed", "error", txErr, "question_id", questionID)
		s.send(state, s.voice(state).tr("Failed to record your answers. Please try again."))
		return -1, false
	}
	if affected == 0 {
		return s.pendingQuestionCount(pdb), true // already answered elsewhere
	}

	s.deps.Bus.Publish(events.NewEvent(events.EventQuestionAnswered, state.ActiveProjectID, map[string]any{
		"question_id": questionID,
		"answer":      summary,
	}))
	return s.pendingQuestionCount(pdb), true
}
