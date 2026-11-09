package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openfabrica/bot"
	"openfabrica/db"
	"openfabrica/events"
)

// QuestionsHandler handles questions listing and answering.
type QuestionsHandler struct {
	deps *Deps
}

type question struct {
	ID         int       `json:"id"`
	ProjectID  int       `json:"project_id"`
	Question   string    `json:"question"`
	Answer     *string   `json:"answer"`
	Status     string    `json:"status"`
	Type       *string   `json:"type,omitempty"`
	SecretName *string   `json:"secret_name,omitempty"`
	Fields     *string   `json:"fields,omitempty"`
	Context    *string   `json:"context,omitempty"`
	Options    *string   `json:"options,omitempty"`
	Urgency    string    `json:"urgency,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// List returns questions across all projects, optionally filtered by status.
func (h *QuestionsHandler) List(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")

	// Get all project IDs from global DB.
	projectRows, err := h.deps.DB.Query("SELECT id FROM projects ORDER BY id")
	if err != nil {
		writeJSON(w, http.StatusOK, []question{})
		return
	}
	projectIDs := collectRows(projectRows, func(rows *sql.Rows) (int, error) {
		var id int
		if err := rows.Scan(&id); err != nil {
			return id, err
		}
		return id, nil
	})

	var questions []question
	for _, projectID := range projectIDs {
		projectDB, err := h.deps.ProjectDBManager.Open(projectID)
		if err != nil {
			continue
		}
		var rows *sql.Rows
		if statusFilter != "" {
			rows, err = projectDB.Query(
				`SELECT q.id, q.question,
				        (SELECT a.answer FROM of_answers a WHERE a.question_id = q.id ORDER BY a.created_at DESC LIMIT 1),
				        q.status, q.type, q.secret_name, q.fields, q.context, q.options, COALESCE(q.urgency,'normal'), q.created_at, COALESCE(q.updated_at, q.created_at)
				 FROM of_questions q WHERE q.status = ? ORDER BY q.created_at DESC`,
				statusFilter,
			)
		} else {
			rows, err = projectDB.Query(
				`SELECT q.id, q.question,
				        (SELECT a.answer FROM of_answers a WHERE a.question_id = q.id ORDER BY a.created_at DESC LIMIT 1),
				        q.status, q.type, q.secret_name, q.fields, q.context, q.options, COALESCE(q.urgency,'normal'), q.created_at, COALESCE(q.updated_at, q.created_at)
				 FROM of_questions q ORDER BY q.created_at DESC`,
			)
		}
		if err != nil {
			continue
		}
		questions = append(questions, collectRows(rows, func(rows *sql.Rows) (question, error) {
			var q question
			q.ProjectID = projectID
			if err := rows.Scan(&q.ID, &q.Question, &q.Answer, &q.Status, &q.Type, &q.SecretName, &q.Fields, &q.Context, &q.Options, &q.Urgency, &q.CreatedAt, &q.UpdatedAt); err != nil {
				return q, err
			}
			return q, nil
		})...)
	}

	if questions == nil {
		questions = []question{}
	}

	writeJSON(w, http.StatusOK, questions)
}

// ListByProject returns questions for a specific project, optionally filtered by status.
func (h *QuestionsHandler) ListByProject(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	statusFilter := r.URL.Query().Get("status")

	var (
		rows *sql.Rows
		err  error
	)

	if statusFilter != "" {
		rows, err = projectDB.Query(
			`SELECT q.id, q.question, q.context, q.options, q.urgency, q.status, q.created_at, q.type, q.secret_name, q.fields,
			        (SELECT a.answer FROM of_answers a WHERE a.question_id = q.id ORDER BY a.created_at DESC LIMIT 1) as answer
			 FROM of_questions q WHERE q.status = ? ORDER BY q.created_at DESC`,
			statusFilter,
		)
	} else {
		rows, err = projectDB.Query(
			`SELECT q.id, q.question, q.context, q.options, q.urgency, q.status, q.created_at, q.type, q.secret_name, q.fields,
			        (SELECT a.answer FROM of_answers a WHERE a.question_id = q.id ORDER BY a.created_at DESC LIMIT 1) as answer
			 FROM of_questions q ORDER BY q.created_at DESC`,
		)
	}

	if err != nil {
		writeJSON(w, http.StatusOK, []projectQuestion{})
		return
	}

	questions := collectRows(rows, func(rows *sql.Rows) (projectQuestion, error) {
		var q projectQuestion
		q.ProjectID = projectID
		if err := rows.Scan(&q.ID, &q.Question, &q.Context, &q.Options, &q.Urgency, &q.Status, &q.CreatedAt, &q.Type, &q.SecretName, &q.Fields, &q.Answer); err != nil {
			return q, err
		}
		return q, nil
	})

	if questions == nil {
		questions = []projectQuestion{}
	}

	writeJSON(w, http.StatusOK, questions)
}

type projectQuestion struct {
	ID         int       `json:"id"`
	ProjectID  int       `json:"project_id"`
	Question   string    `json:"question"`
	Context    *string   `json:"context"`
	Options    *string   `json:"options"`
	Urgency    string    `json:"urgency"`
	Status     string    `json:"status"`
	Answer     *string   `json:"answer"`
	CreatedAt  time.Time `json:"created_at"`
	Type       *string   `json:"type,omitempty"`
	SecretName *string   `json:"secret_name,omitempty"`
	Fields     *string   `json:"fields,omitempty"`
}

type answerRequest struct {
	Answer    string `json:"answer"`
	ProjectID int    `json:"project_id"`
	Deferred  bool   `json:"deferred"`
}

// DeferredAnswerText is the sentinel stored in of_answers when the owner
// opts out of a question. It flows through buildPlanUserMessage so the LLM
// understands the value will arrive later and avoids immediately re-asking.
const DeferredAnswerText = bot.DeferredAnswerText

// Answer provides an answer to a pending question.
func (h *QuestionsHandler) Answer(w http.ResponseWriter, r *http.Request) {
	questionID, ok := requireIntParam(w, r, "questionID", "invalid question ID")
	if !ok {
		return
	}

	var req answerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if !req.Deferred && req.Answer == "" {
		writeError(w, http.StatusBadRequest, "answer is required")
		return
	}

	// Resolve which project DB contains this question.
	projectID := req.ProjectID
	if projectID == 0 {
		// Search across all project DBs for the question.
		for _, id := range h.deps.ProjectDBManager.OpenProjectIDs() {
			sdb, err := h.deps.ProjectDBManager.Open(id)
			if err != nil {
				continue
			}
			var exists int
			if sdb.QueryRow("SELECT 1 FROM of_questions WHERE id = ?", questionID).Scan(&exists) == nil {
				projectID = id
				break
			}
		}
	}
	if projectID == 0 {
		writeError(w, http.StatusNotFound, "question not found")
		return
	}

	projectDB := openProjectDB(w, h.deps.ProjectDBManager, projectID)
	if projectDB == nil {
		return
	}
	// The answer wakes the engine below, and its next message is written for whoever answered.
	h.deps.recordReplyLanguage(r, projectID)

	// Check if this is a secret-type question or has structured fields.
	var qType, secretName, fieldsJSON, qStatus sql.NullString
	if err := projectDB.QueryRow(
		"SELECT type, secret_name, fields, status FROM of_questions WHERE id = ?", questionID,
	).Scan(&qType, &secretName, &fieldsJSON, &qStatus); err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "question not found")
		return
	} else if err != nil {
		h.deps.Logger.Error("failed to load question metadata", "question_id", questionID, "error", err)
	}
	// A second answer (two tabs, the admin and Telegram racing) must not store
	// secrets again or wake the engine twice. A deferred question can still be
	// answered for real later.
	if qStatus.Valid && qStatus.String != "pending" && qStatus.String != "deferred" {
		writeError(w, http.StatusConflict, "this question has already been answered")
		return
	}

	// Deferred answers skip all secret encryption / field processing and just
	// record the sentinel. The owner can fill in the real value later from the
	// questions panel; the engine treats 'deferred' as "no longer pending" so
	// the pipeline resumes.
	newStatus := "answered"
	if req.Deferred {
		req.Answer = DeferredAnswerText
		newStatus = "deferred"
	} else {
		// Approvals stash arbitrary preview content in the fields column, skip JSON parse for them.
		isApproval := qType.Valid && qType.String == "approval"
		// The "null" check is defensive: a typed-nil slice marshals to the
		// literal string "null" for an omitted fields arg, which must not be
		// treated as a real form payload.
		hasFields := fieldsJSON.Valid && fieldsJSON.String != "" && fieldsJSON.String != "[]" && fieldsJSON.String != "null" && !isApproval
		if hasFields {
			req.Answer = h.processFieldAnswer(projectDB, projectID, questionID, fieldsJSON.String, req.Answer)
		} else if qType.Valid && qType.String == "secret" && secretName.Valid && secretName.String != "" {
			// Single secret-type question (no fields, just one encrypted answer).
			encrypted, encErr := h.deps.Encryptor.Encrypt(req.Answer)
			if encErr != nil {
				h.deps.serverError(w, "failed to encrypt secret", encErr)
				return
			}
			if err := h.storeSecret(projectDB, secretName.String, encrypted, projectID, questionID); err != nil {
				h.deps.Logger.Error("failed to store secret", "name", secretName.String, "error", err)
			}
			req.Answer = fmt.Sprintf("Secret '%s' configured", secretName.String)
		}
	}

	// Status change and answer record land together; the status guard makes a
	// racing second answer affect no rows.
	var claimed bool
	if err := projectDB.WithTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"UPDATE of_questions SET status = ?, updated_at = datetime('now') WHERE id = ? AND status IN ('pending','deferred')",
			newStatus, questionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		claimed = true
		_, err = tx.Exec("INSERT INTO of_answers (question_id, answer) VALUES (?, ?)", questionID, req.Answer)
		return err
	}); err != nil {
		h.deps.Logger.Error("failed to record answer", "question_id", questionID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record answer")
		return
	}
	if !claimed {
		writeError(w, http.StatusConflict, "this question has already been answered")
		return
	}

	// Wake the engine immediately when any question is answered.
	// The engine pauses after each question, it should resume per-answer.
	if h.deps.Bus != nil {
		// Fetch this question's Q+A text for the wake payload.
		var answerText string
		row := projectDB.QueryRowWriter(
			`SELECT q.question || ': ' || a.answer FROM of_questions q
			 JOIN of_answers a ON a.question_id = q.id
			 WHERE q.id = ? ORDER BY a.created_at DESC LIMIT 1`, questionID)
		if row.Scan(&answerText) != nil {
			answerText = req.Answer
		}
		h.deps.Bus.Publish(events.NewEvent(events.EventQuestionAnswered, projectID, map[string]any{
			"question_id": questionID,
			"answer":      answerText,
			"deferred":    req.Deferred,
		}))
	}

	// Count remaining pending questions for the response.
	var pendingCount int
	row := projectDB.QueryRowWriter("SELECT COUNT(*) FROM of_questions WHERE status = 'pending'")
	if err := row.Scan(&pendingCount); err != nil {
		pendingCount = 0
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      questionID,
		"status":  newStatus,
		"pending": pendingCount,
	})
}

// storeSecret upserts an encrypted secret and publishes an event.
func (h *QuestionsHandler) storeSecret(projectDB *db.ProjectDB, secretName, encrypted string, projectID, questionID int) error {
	_, err := projectDB.ExecWrite(
		`INSERT INTO of_secrets (name, value_encrypted, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
		secretName, encrypted,
	)
	if err != nil {
		return err
	}
	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventSecretStored, projectID, map[string]any{
			"name":        secretName,
			"question_id": questionID,
		}))
	}
	return nil
}

// processFieldAnswer handles structured multi-field answers, storing secrets
// and memory values for each field. Returns the summary answer string.
func (h *QuestionsHandler) processFieldAnswer(projectDB *db.ProjectDB, projectID, questionID int, fieldsJSONStr, rawAnswer string) string {
	var fieldDefs []struct {
		Name       string `json:"name"`
		Label      string `json:"label"`
		Type       string `json:"type"`
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal([]byte(fieldsJSONStr), &fieldDefs); err != nil {
		h.deps.Logger.Error("failed to parse field definitions", "question_id", questionID, "error", err)
		return rawAnswer
	}

	var fieldValues map[string]string
	if err := json.Unmarshal([]byte(rawAnswer), &fieldValues); err != nil {
		// The client should submit a JSON object keyed by field name, but a
		// malformed form widget (or a hand-submitted answer) can send a bare
		// string. Degrade instead of dropping the answer: a single-field form
		// can treat the raw string as that field's value (so its secret/memory
		// still gets configured); multi-field forms keep the raw text so it is
		// at least visible in of_answers. Either way the pipeline still resumes.
		h.deps.Logger.Warn("field values not JSON, falling back to raw", "question_id", questionID, "error", err)
		if len(fieldDefs) == 1 {
			fieldValues = map[string]string{fieldDefs[0].Name: rawAnswer}
		} else {
			return rawAnswer
		}
	}

	var configured []string
	for _, fd := range fieldDefs {
		val, ok := fieldValues[fd.Name]
		if !ok || val == "" {
			continue
		}
		if fd.Type == "secret" && fd.SecretName != "" {
			encrypted, encErr := h.deps.Encryptor.Encrypt(val)
			if encErr != nil {
				h.deps.Logger.Error("failed to encrypt field secret", "field", fd.Name, "error", encErr)
				continue
			}
			if err := h.storeSecret(projectDB, fd.SecretName, encrypted, projectID, questionID); err != nil {
				h.deps.Logger.Error("failed to store field secret", "field", fd.Name, "error", err)
				continue
			}
		} else {
			if _, err := projectDB.ExecWrite(
				`INSERT INTO of_memory (key, value, category) VALUES (?, ?, 'field_answer')
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
				fd.Name, val,
			); err != nil {
				h.deps.Logger.Error("failed to store field memory", "field", fd.Name, "error", err)
				continue
			}
		}
		configured = append(configured, fd.Label)
	}
	return "Configured: " + strings.Join(configured, ", ")
}
