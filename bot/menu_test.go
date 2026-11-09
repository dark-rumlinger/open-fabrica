package bot

import (
	"fmt"
	"strings"
	"testing"

	"openfabrica/db/models"
)

// seedProject inserts a project row into the global DB and returns its id. Model
// id 0 is fine: the settings screen renders it as "-".
func seedProject(t *testing.T, svc *BotService, name string) int {
	t.Helper()
	p, err := models.CreateProject(svc.deps.DB.DB, name, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p.ID
}

// authedUser authorizes a user without pointing it at a project (so selection
// behavior can be observed).
func authedUser(t *testing.T, svc *BotService) *UserState {
	t.Helper()
	if err := svc.deps.DB.SetSetting("telegram_allowed_users", "999"); err != nil {
		t.Fatalf("set allowed users: %v", err)
	}
	st := svc.getOrCreateUser("telegram", "555")
	st.UserID = "999"
	SaveSession(svc.deps.DB, st)
	return st
}

// TestNavProjectsEditsInPlace: tapping a nav button edits the same message
// rather than posting a new one.
func TestNavProjectsEditsInPlace(t *testing.T) {
	svc, fp, _ := newTestService(t)
	authedUser(t, svc)
	seedProject(t, svc, "Acme")

	svc.HandleCallback("telegram", "555", "999", 100, "nav:projects")

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.edits) != 1 || fp.edits[0].id != 100 {
		t.Fatalf("expected one in-place edit of message 100, got %+v", fp.edits)
	}
	if len(fp.messages) != 0 {
		t.Errorf("expected no fresh messages, got %v", fp.messages)
	}
	if !strings.Contains(fp.edits[0].text, "Your projects") {
		t.Errorf("edit text = %q, want the projects list", fp.edits[0].text)
	}
}

// TestSelectProjectRendersChat: selecting a project sets ActiveProjectID and
// lands the user on the project's chat screen.
func TestSelectProjectRendersChat(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := authedUser(t, svc)
	pid := seedProject(t, svc, "Acme")

	svc.HandleCallback("telegram", "555", "999", 100, fmt.Sprintf("project:%d", pid))

	if st.ActiveProjectID != pid {
		t.Fatalf("ActiveProjectID = %d, want %d", st.ActiveProjectID, pid)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.edits) == 0 {
		t.Fatalf("expected an in-place edit into the chat screen, got none")
	}
	last := fp.edits[len(fp.edits)-1].text
	if !strings.Contains(last, "Chatting with") || !strings.Contains(last, "Acme") {
		t.Errorf("chat screen text = %q, want a 'Chatting with Acme' header", last)
	}
}

// TestNavSettingsRenders: the settings screen shows the project info.
func TestNavSettingsRenders(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := authedUser(t, svc)
	pid := seedProject(t, svc, "Acme")
	st.ActiveProjectID = pid
	SaveSession(svc.deps.DB, st)

	svc.HandleCallback("telegram", "555", "999", 100, "nav:settings")

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.edits) != 1 {
		t.Fatalf("expected one settings edit, got %+v", fp.edits)
	}
	if !strings.Contains(fp.edits[0].text, "Settings") {
		t.Errorf("settings text = %q, want a Settings view", fp.edits[0].text)
	}
}

// TestNavSettingsWithoutProject bounces to the main menu.
func TestNavSettingsWithoutProject(t *testing.T) {
	svc, fp, _ := newTestService(t)
	authedUser(t, svc) // no active project

	toast := svc.HandleCallback("telegram", "555", "999", 100, "nav:settings")

	if toast == "" {
		t.Errorf("expected a 'pick a project' toast")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.edits) != 1 || !strings.Contains(fp.edits[0].text, "Open Fabrica") {
		t.Errorf("expected an edit to the main menu, got %+v", fp.edits)
	}
}

// TestAwaitingRenameUpdatesName: the rename flow persists the new name.
func TestAwaitingRenameUpdatesName(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := authedUser(t, svc)
	pid := seedProject(t, svc, "OldName")
	st.ActiveProjectID = pid
	st.PendingAction = "awaiting_rename"
	SaveSession(svc.deps.DB, st)

	svc.HandleIncoming("telegram", "555", "999", 5, "NewName")

	p, err := models.GetProjectByID(svc.deps.DB.DB, pid)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.Name != "NewName" {
		t.Errorf("name = %q, want NewName", p.Name)
	}
}

// TestSequentialQuestionAnswering: when the engine asks several questions,
// answering the first records it, shows a progress line that renders its counts
// as numbers rather than raw format verbs, and surfaces the next one.
func TestSequentialQuestionAnswering(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'What is it?', 'pending', 'text')")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (2, 'Who for?', 'pending', 'text')")
	st.PendingAction = "awaiting_answer"
	st.PendingData = map[string]string{"question_id": "1"}
	SaveSession(svc.deps.DB, st)

	svc.HandleIncoming("telegram", "555", "999", 10, "a blog")

	var a1 string
	if err := pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id=1").Scan(&a1); err != nil || a1 != "a blog" {
		t.Fatalf("Q1 answer = %q (err %v), want 'a blog'", a1, err)
	}
	if st.PendingData["question_id"] != "2" {
		t.Fatalf("after answering Q1, active question = %q, want 2", st.PendingData["question_id"])
	}

	fp.mu.Lock()
	var progress string
	for _, m := range fp.messages {
		if strings.Contains(m, "question(s) remaining") {
			progress = m
		}
	}
	fp.mu.Unlock()
	if !strings.Contains(progress, "1 question(s) remaining") {
		t.Errorf("progress message = %q, want a clean '1 question(s) remaining'", progress)
	}
	if strings.Contains(progress, "%!") {
		t.Errorf("progress message has a format bug: %q", progress)
	}

	svc.HandleIncoming("telegram", "555", "999", 11, "my dog")
	var a2 string
	if err := pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id=2").Scan(&a2); err != nil || a2 != "my dog" {
		t.Fatalf("Q2 answer = %q (err %v), want 'my dog'", a2, err)
	}
}

// TestQuestionBatchDeliversOneAtATime: two near-simultaneous question events must
// surface only the first question, not stack both.
func TestQuestionBatchDeliversOneAtATime(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)
	st.ActiveProjectID = 1
	SaveSession(svc.deps.DB, st)
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'First?', 'pending', 'text')")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (2, 'Second?', 'pending', 'text')")

	// Deliver both (as onQuestionAsked would, once per event) - with the guard,
	// only the first should be sent.
	st.Mu.Lock()
	svc.deliverNextIfIdle(st)
	svc.deliverNextIfIdle(st)
	st.Mu.Unlock()

	fp.mu.Lock()
	defer fp.mu.Unlock()
	delivered := 0
	// Text questions carry a "Later" button, so they go out as keyboard messages.
	for _, m := range append(append([]string{}, fp.messages...), fp.questions...) {
		if strings.Contains(m, "First?") || strings.Contains(m, "Second?") {
			delivered++
		}
	}
	if delivered != 1 {
		t.Errorf("expected exactly one question delivered at a time, got %d (%v %v)", delivered, fp.messages, fp.questions)
	}
	if st.PendingData["question_id"] != "1" {
		t.Errorf("active question = %q, want the oldest (1)", st.PendingData["question_id"])
	}
}

// TestEditFailureFallsBackToSend: when the message can't be edited (stale), the
// nav layer posts a fresh message instead.
func TestEditFailureFallsBackToSend(t *testing.T) {
	svc, fp, _ := newTestService(t)
	authedUser(t, svc)
	fp.failEdit = true

	svc.HandleCallback("telegram", "555", "999", 100, "nav:menu")

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.edits) != 0 {
		t.Errorf("edit should have failed, recorded %+v", fp.edits)
	}
	if len(fp.questions) != 1 {
		t.Errorf("expected a fresh keyboard message as fallback, got questions=%v", fp.questions)
	}
}
