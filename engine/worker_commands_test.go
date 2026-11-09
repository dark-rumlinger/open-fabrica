package engine

import (
	"strings"
	"testing"
)

// A skipped question reaches the planner as the sentinel, and the block says what
// the sentinel means so the model neither waits on it nor asks again.
func TestCollectAnswersExplainsDeferredSentinel(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	res, err := pdb.ExecWrite(`INSERT INTO of_questions (question, status) VALUES ('Brand colour?', 'deferred')`)
	if err != nil {
		t.Fatalf("insert question: %v", err)
	}
	qid, _ := res.LastInsertId()
	if _, err := pdb.ExecWrite(`INSERT INTO of_answers (question_id, answer, answered_by) VALUES (?, ?, 'owner')`, qid, DeferredAnswerText); err != nil {
		t.Fatalf("insert answer: %v", err)
	}
	got := w.collectAnswers()
	if !strings.Contains(got, "Brand colour?: "+DeferredAnswerText) {
		t.Errorf("the deferred answer should be listed, got %q", got)
	}
	if !strings.Contains(got, "do not ask it again") {
		t.Errorf("the sentinel must be explained, got %q", got)
	}
}
