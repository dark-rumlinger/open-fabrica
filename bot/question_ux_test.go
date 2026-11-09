package bot

import (
	"strings"
	"testing"
)

func lastKeyboard(t *testing.T, fp *fakeProvider) keyboard {
	t.Helper()
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.keyboards) == 0 {
		t.Fatal("no question keyboard was sent")
	}
	return fp.keyboards[len(fp.keyboards)-1]
}

func answerOf(t *testing.T, svc *BotService, qid int) (string, bool) {
	t.Helper()
	pdb := svc.deps.ProjectDBManager.Get(1)
	var a string
	err := pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id = ?", qid).Scan(&a)
	return a, err == nil
}

// After /questions -> open question 3, a typed reply belongs to question 3, not
// to whichever question happens to be oldest.
func TestTypedReplyAnswersTheQuestionThatWasOpened(t *testing.T) {
	svc, _, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'First?', 'pending', 'text')")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (3, 'Third?', 'pending', 'text')")

	st.Mu.Lock()
	svc.promptQuestionByID(st, "3")
	st.Mu.Unlock()
	svc.HandleIncoming("telegram", "555", "999", 10, "third answer")

	if a, ok := answerOf(t, svc, 3); !ok || a != "third answer" {
		t.Fatalf("question 3 answer = %q (recorded=%v), want 'third answer'", a, ok)
	}
	if _, ok := answerOf(t, svc, 1); ok {
		t.Fatal("question 1 must stay unanswered")
	}
}

func TestQuestionShowsPositionContextAndUrgency(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'First?', 'pending', 'text')")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, context, urgency, status, type) VALUES (2, 'Which <domain>?', 'Needed before DNS goes out', 'high', 'pending', 'text')")

	st.Mu.Lock()
	svc.promptQuestionByID(st, "2")
	st.Mu.Unlock()

	fp.mu.Lock()
	defer fp.mu.Unlock()
	got := fp.questions[len(fp.questions)-1]
	for _, want := range []string{"Question 2 of 2", "🔴", "Needed before DNS goes out", "Which &lt;domain&gt;?"} {
		if !strings.Contains(got, want) {
			t.Errorf("question message missing %q:\n%s", want, got)
		}
	}
}

func TestApprovalOptionsBecomeButtons(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite("INSERT INTO of_questions (id, question, context, options, status, type) VALUES (1, 'Drop the orders table?', 'approval:schema_drop', 'Approve, Deny', 'pending', 'approval')")

	st.Mu.Lock()
	svc.promptQuestionByID(st, "1")
	st.Mu.Unlock()

	kb := lastKeyboard(t, fp)
	if len(kb.labels) < 2 || kb.labels[0] != "Approve" || kb.labels[1] != "Deny" {
		t.Fatalf("labels = %v, want Approve and Deny buttons first", kb.labels)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if strings.Contains(fp.questions[len(fp.questions)-1], "approval:") {
		t.Error("the internal approval tag must not be shown to the owner")
	}
}

func TestMultiChoiceToggleThenDone(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite(`INSERT INTO of_questions (id, question, options, status, type) VALUES (1, 'Which channels?', '["Email","SMS","Push"]', 'pending', 'multi_choice')`)

	st.Mu.Lock()
	svc.promptQuestionByID(st, "1")
	st.Mu.Unlock()

	kb := lastKeyboard(t, fp)
	if kb.cbs[0] != "m:1:0" || kb.cbs[3] != "m:1:done" {
		t.Fatalf("callbacks = %v, want toggle buttons then a Done button", kb.cbs)
	}

	svc.HandleCallback("telegram", "555", "999", 77, "m:1:0")
	svc.HandleCallback("telegram", "555", "999", 77, "m:1:2")
	fp.mu.Lock()
	if n := len(fp.edits); n != 2 || !strings.Contains(fp.edits[n-1].markup, "✅ Email") || !strings.Contains(fp.edits[n-1].markup, "✅ Push") {
		t.Fatalf("keyboard must be redrawn with the selection ticked, edits=%+v", fp.edits)
	}
	fp.mu.Unlock()

	// Tapping a ticked option clears it.
	svc.HandleCallback("telegram", "555", "999", 77, "m:1:0")
	svc.HandleCallback("telegram", "555", "999", 77, "m:1:done")

	if a, ok := answerOf(t, svc, 1); !ok || a != "Push" {
		t.Fatalf("answer = %q (recorded=%v), want 'Push'", a, ok)
	}
}

func TestMultiChoiceDoneNeedsASelection(t *testing.T) {
	svc, _, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite(`INSERT INTO of_questions (id, question, options, status, type) VALUES (1, 'Which?', '["A","B"]', 'pending', 'multi_choice')`)
	st.Mu.Lock()
	svc.promptQuestionByID(st, "1")
	st.Mu.Unlock()

	if toast := svc.HandleCallback("telegram", "555", "999", 77, "m:1:done"); toast != "Pick at least one option" {
		t.Fatalf("toast = %q", toast)
	}
	if _, ok := answerOf(t, svc, 1); ok {
		t.Fatal("an empty selection must not be recorded")
	}
}

func TestParseOptionsJSONForms(t *testing.T) {
	cases := map[string][]string{
		`["a","b"]`:                     {"a", "b"},
		`[{"label":"A"},{"value":"B"}]`: {"A", "B"},
		`Approve, Deny`:                 {"Approve", "Deny"},
		`null`:                          nil,
	}
	for in, want := range cases {
		got := parseOptionsJSON(in)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("parseOptionsJSON(%q) = %v, want %v", in, got, want)
		}
	}
}

// After a restart the owner may never have seen a question, so the bot shows the
// oldest one again with a count, but leaves a user who is mid-form alone.
func TestResendPendingQuestionsAfterRestart(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	armedUser(t, svc)
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'First?', 'pending', 'text')")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (2, 'Second?', 'pending', 'text')")

	svc.resendPendingQuestions()

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.messages) == 0 || !strings.Contains(fp.messages[0], "2 question(s) still waiting") {
		t.Fatalf("expected a count message, got %v", fp.messages)
	}
	if len(fp.questions) != 1 || !strings.Contains(fp.questions[0], "First?") {
		t.Fatalf("expected only the oldest question, got %v", fp.questions)
	}
}

func TestResendSkipsUsersInAFormAndEmptyQueues(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)

	svc.resendPendingQuestions() // nothing pending
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'First?', 'pending', 'text')")
	st.PendingAction = "awaiting_form"
	svc.resendPendingQuestions() // mid-form

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.messages) != 0 || len(fp.questions) != 0 {
		t.Fatalf("nothing should be sent, got messages=%v questions=%v", fp.messages, fp.questions)
	}
}
