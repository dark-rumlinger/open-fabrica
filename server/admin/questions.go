package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"openfabrica/bot"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/store"
)

// QuestionsHandler handles questions listing and answering.
type QuestionsHandler struct {
	deps *Deps
}

// List returns questions across all projects, optionally filtered by status.
func (h *QuestionsHandler) List(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")

	projects, err := models.ListProjects(h.deps.DB.DB)
	if err != nil {
		writeJSON(w, http.StatusOK, []store.QuestionRecord{})
		return
	}

	questions := []store.QuestionRecord{}
	for _, p := range projects {
		projectDB, err := h.deps.ProjectDBManager.Open(p.ID)
		if err != nil {
			continue
		}
		list, err := store.ListQuestions(projectDB, p.ID, statusFilter)
		if err != nil {
			continue
		}
		questions = append(questions, list...)
	}

	writeJSON(w, http.StatusOK, questions)
}

// ListByProject returns questions for a specific project, optionally filtered by status.
func (h *QuestionsHandler) ListByProject(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	questions, err := store.ListQuestions(projectDB, projectID, r.URL.Query().Get("status"))
	if err != nil {
		writeJSON(w, http.StatusOK, []store.QuestionRecord{})
		return
	}
	writeJSON(w, http.StatusOK, questions)
}

type answerRequest struct {
	Answer    string `json:"answer"`
	ProjectID int    `json:"project_id"`
	Deferred  bool   `json:"deferred"`
}

// DeferredAnswerText is the sentinel stored in of_answers when the owner
// opts out of a question.
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

	projectID := req.ProjectID
	if projectID == 0 {
		for _, id := range h.deps.ProjectDBManager.OpenProjectIDs() {
			sdb, err := h.deps.ProjectDBManager.Open(id)
			if err != nil {
				continue
			}
			if store.QuestionExists(sdb, questionID) {
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
	h.deps.recordReplyLanguage(r, projectID)

	meta, err := store.LoadQuestionMeta(projectDB, questionID)
	if errors.Is(err, store.ErrQuestionNotFound) {
		writeError(w, http.StatusNotFound, "question not found")
		return
	}
	if err != nil {
		h.deps.Logger.Error("failed to load question metadata", "question_id", questionID, "error", err)
		meta = &store.QuestionMeta{}
	}
	if meta.Status != "" && meta.Status != "pending" && meta.Status != "deferred" {
		writeError(w, http.StatusConflict, "this question has already been answered")
		return
	}

	newStatus := "answered"
	if req.Deferred {
		req.Answer = DeferredAnswerText
		newStatus = "deferred"
	} else {
		isApproval := meta.Type == "approval"
		hasFields := meta.Fields != "" && meta.Fields != "[]" && meta.Fields != "null" && !isApproval
		if hasFields {
			req.Answer = h.processFieldAnswer(projectDB, projectID, questionID, meta.Fields, req.Answer)
		} else if meta.Type == "secret" && meta.SecretName != "" {
			encrypted, encErr := h.deps.Encryptor.Encrypt(req.Answer)
			if encErr != nil {
				h.deps.serverError(w, "failed to encrypt secret", encErr)
				return
			}
			if err := h.storeSecret(projectDB, meta.SecretName, encrypted, projectID, questionID); err != nil {
				h.deps.Logger.Error("failed to store secret", "name", meta.SecretName, "error", err)
			}
			req.Answer = fmt.Sprintf("Secret '%s' configured", meta.SecretName)
		}
	}

	claimed, err := store.ClaimAndRecordAnswer(projectDB, questionID, newStatus, req.Answer)
	if err != nil {
		h.deps.Logger.Error("failed to record answer", "question_id", questionID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record answer")
		return
	}
	if !claimed {
		writeError(w, http.StatusConflict, "this question has already been answered")
		return
	}

	if h.deps.Bus != nil {
		answerText := store.LoadQuestionAnswerSummary(projectDB, questionID)
		if answerText == "" {
			answerText = req.Answer
		}
		h.deps.Bus.Publish(events.NewEvent(events.EventQuestionAnswered, projectID, map[string]any{
			"question_id": questionID,
			"answer":      answerText,
			"deferred":    req.Deferred,
		}))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      questionID,
		"status":  newStatus,
		"pending": store.CountPendingQuestions(projectDB),
	})
}

func (h *QuestionsHandler) storeSecret(projectDB *db.ProjectDB, secretName, encrypted string, projectID, questionID int) error {
	if err := store.UpsertEncryptedSecret(projectDB, secretName, encrypted); err != nil {
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
			if err := store.UpsertFieldAnswerMemory(projectDB, fd.Name, val); err != nil {
				h.deps.Logger.Error("failed to store field memory", "field", fd.Name, "error", err)
				continue
			}
		}
		configured = append(configured, fd.Label)
	}
	return "Configured: " + strings.Join(configured, ", ")
}
