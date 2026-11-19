package store

import (
	"database/sql"
	"errors"
	"time"

	"openfabrica/db"
)

// ErrQuestionNotFound is returned when a question row does not exist.
var ErrQuestionNotFound = errors.New("question not found")

// QuestionRecord represents a row in of_questions with its latest answer.
type QuestionRecord struct {
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

// QuestionMeta holds metadata needed when answering a question.
type QuestionMeta struct {
	Type       string
	SecretName string
	Fields     string
	Status     string
}

// ListQuestions returns questions for a project, optionally filtered by status.
func ListQuestions(pdb *db.ProjectDB, projectID int, statusFilter string) ([]QuestionRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if statusFilter != "" {
		rows, err = pdb.Query(
			`SELECT q.id, q.question,
			        (SELECT a.answer FROM of_answers a WHERE a.question_id = q.id ORDER BY a.created_at DESC LIMIT 1),
			        q.status, q.type, q.secret_name, q.fields, q.context, q.options, COALESCE(q.urgency,'normal'), q.created_at, COALESCE(q.updated_at, q.created_at)
			 FROM of_questions q WHERE q.status = ? ORDER BY q.created_at DESC`,
			statusFilter,
		)
	} else {
		rows, err = pdb.Query(
			`SELECT q.id, q.question,
			        (SELECT a.answer FROM of_answers a WHERE a.question_id = q.id ORDER BY a.created_at DESC LIMIT 1),
			        q.status, q.type, q.secret_name, q.fields, q.context, q.options, COALESCE(q.urgency,'normal'), q.created_at, COALESCE(q.updated_at, q.created_at)
			 FROM of_questions q ORDER BY q.created_at DESC`,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []QuestionRecord{}
	for rows.Next() {
		var q QuestionRecord
		q.ProjectID = projectID
		if rows.Scan(&q.ID, &q.Question, &q.Answer, &q.Status, &q.Type, &q.SecretName, &q.Fields, &q.Context, &q.Options, &q.Urgency, &q.CreatedAt, &q.UpdatedAt) == nil {
			out = append(out, q)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// QuestionExists reports whether questionID exists in pdb.
func QuestionExists(pdb *db.ProjectDB, questionID int) bool {
	var exists int
	return pdb.QueryRow("SELECT 1 FROM of_questions WHERE id = ?", questionID).Scan(&exists) == nil
}

// LoadQuestionMeta loads type, secret_name, fields, and status for questionID.
func LoadQuestionMeta(pdb *db.ProjectDB, questionID int) (*QuestionMeta, error) {
	var qType, secretName, fieldsJSON, qStatus sql.NullString
	err := pdb.QueryRow(
		"SELECT type, secret_name, fields, status FROM of_questions WHERE id = ?", questionID,
	).Scan(&qType, &secretName, &fieldsJSON, &qStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrQuestionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &QuestionMeta{
		Type:       qType.String,
		SecretName: secretName.String,
		Fields:     fieldsJSON.String,
		Status:     qStatus.String,
	}, nil
}

// ClaimAndRecordAnswer atomically transitions a pending/deferred question to newStatus
// and inserts the answer row. Returns claimed=false if another caller already answered it.
func ClaimAndRecordAnswer(pdb *db.ProjectDB, questionID int, newStatus, answer string) (claimed bool, err error) {
	err = pdb.WithTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"UPDATE of_questions SET status = ?, updated_at = datetime('now') WHERE id = ? AND status IN ('pending','deferred')",
			newStatus, questionID,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		claimed = true
		_, err = tx.Exec("INSERT INTO of_answers (question_id, answer) VALUES (?, ?)", questionID, answer)
		return err
	})
	return claimed, err
}

// LoadQuestionAnswerSummary returns "<question>: <answer>" for the latest answer, or "".
func LoadQuestionAnswerSummary(pdb *db.ProjectDB, questionID int) string {
	var text string
	err := pdb.QueryRowWriter(
		`SELECT q.question || ': ' || a.answer FROM of_questions q
		 JOIN of_answers a ON a.question_id = q.id
		 WHERE q.id = ? ORDER BY a.created_at DESC LIMIT 1`,
		questionID,
	).Scan(&text)
	if err != nil {
		return ""
	}
	return text
}

// CountPendingQuestions returns the number of unanswered pending questions.
func CountPendingQuestions(pdb *db.ProjectDB) int {
	var n int
	if err := pdb.QueryRowWriter("SELECT COUNT(*) FROM of_questions WHERE status = 'pending'").Scan(&n); err != nil {
		return 0
	}
	return n
}

// UpsertEncryptedSecret inserts or updates an encrypted secret in of_secrets.
func UpsertEncryptedSecret(pdb *db.ProjectDB, name, encryptedValue string) error {
	_, err := pdb.ExecWrite(
		`INSERT INTO of_secrets (name, value_encrypted, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
		name, encryptedValue,
	)
	return err
}

// UpsertFieldAnswerMemory stores a non-secret structured field answer in of_memory.
func UpsertFieldAnswerMemory(pdb *db.ProjectDB, key, value string) error {
	_, err := pdb.ExecWrite(
		`INSERT INTO of_memory (key, value, category) VALUES (?, ?, 'field_answer')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value,
	)
	return err
}
