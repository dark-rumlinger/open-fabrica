package admin

import (
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
)

type questionsFixture struct {
	router   chi.Router
	pdb      *db.ProjectDB
	answered *[]events.Event
}

func newQuestionsFixture(t *testing.T) *questionsFixture {
	t.Helper()
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	bus := events.NewBus()
	var answered []events.Event
	bus.Subscribe(events.EventQuestionAnswered, func(e events.Event) { answered = append(answered, e) })

	deps := &Deps{Core: sdeps.Core{
		DB:               &db.DB{DB: mem},
		ProjectDBManager: mgr,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Bus:              bus,
		Encryptor:        enc,
	}}
	h := &QuestionsHandler{deps: deps}
	r := chi.NewRouter()
	r.Post("/questions/{questionID}/answer", h.Answer)
	return &questionsFixture{router: r, pdb: pdb, answered: &answered}
}

func (f *questionsFixture) seed(t *testing.T, q, status, qType, secretName, fields string) int {
	t.Helper()
	res, err := f.pdb.ExecWrite(
		"INSERT INTO of_questions (question, status, type, secret_name, fields) VALUES (?, ?, ?, ?, ?)",
		q, status, qType, secretName, fields)
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func (f *questionsFixture) answer(t *testing.T, id int, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/questions/"+itoa(id)+"/answer", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *questionsFixture) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pdb.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestAnswerRecordsAnswerAndWakesOnce(t *testing.T) {
	f := newQuestionsFixture(t)
	id := f.seed(t, "Which palette?", "pending", "single_choice", "", "")

	if rec := f.answer(t, id, `{"answer":"Ocean","project_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := f.countRows(t, "SELECT COUNT(*) FROM of_questions WHERE id = ? AND status = 'answered'", id); got != 1 {
		t.Fatalf("question must be answered")
	}
	if len(*f.answered) != 1 || (*f.answered)[0].Payload["answer"] != "Which palette?: Ocean" {
		t.Fatalf("expected one wake event carrying the Q and A, got %+v", *f.answered)
	}
}

func TestAnswerTwiceIsConflictAndDoesNotWakeAgain(t *testing.T) {
	f := newQuestionsFixture(t)
	id := f.seed(t, "Which palette?", "pending", "text", "", "")

	f.answer(t, id, `{"answer":"Ocean","project_id":1}`)
	rec := f.answer(t, id, `{"answer":"Forest","project_id":1}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("second answer status = %d, want 409", rec.Code)
	}
	if got := f.countRows(t, "SELECT COUNT(*) FROM of_answers WHERE question_id = ?", id); got != 1 {
		t.Fatalf("answers recorded = %d, want 1", got)
	}
	if len(*f.answered) != 1 {
		t.Fatalf("wake events = %d, want 1", len(*f.answered))
	}
}

func TestAnswerUnknownQuestionIs404(t *testing.T) {
	f := newQuestionsFixture(t)
	if rec := f.answer(t, 9, `{"answer":"x","project_id":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeferredQuestionCanBeAnsweredLater(t *testing.T) {
	f := newQuestionsFixture(t)
	id := f.seed(t, "Brand colour?", "pending", "text", "", "")

	if rec := f.answer(t, id, `{"deferred":true,"project_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("defer status = %d: %s", rec.Code, rec.Body)
	}
	if got := f.countRows(t, "SELECT COUNT(*) FROM of_questions WHERE id = ? AND status = 'deferred'", id); got != 1 {
		t.Fatalf("question must be deferred")
	}
	if rec := f.answer(t, id, `{"answer":"Teal","project_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("late answer status = %d: %s", rec.Code, rec.Body)
	}
	if got := f.countRows(t, "SELECT COUNT(*) FROM of_questions WHERE id = ? AND status = 'answered'", id); got != 1 {
		t.Fatalf("question must be answered after the late answer")
	}
}

func TestSecretAnswerIsEncryptedAndNotStoredInPlain(t *testing.T) {
	f := newQuestionsFixture(t)
	id := f.seed(t, "Stripe key?", "pending", "secret", "STRIPE_KEY", "")

	if rec := f.answer(t, id, `{"answer":"sk_live_123","project_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var stored, plain string
	if err := f.pdb.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = 'STRIPE_KEY'").Scan(&stored); err != nil || stored == "" || stored == "sk_live_123" {
		t.Fatalf("secret must be stored encrypted, got %q (%v)", stored, err)
	}
	_ = f.pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id = ?", id).Scan(&plain)
	if strings.Contains(plain, "sk_live_123") {
		t.Fatalf("the answer record must not hold the secret: %q", plain)
	}
}
