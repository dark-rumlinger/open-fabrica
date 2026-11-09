package bot

import (
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/events"
)

// screenUser is an authorized user pointed at a seeded project 1.
func screenUser(t *testing.T, svc *BotService, name string) *UserState {
	t.Helper()
	st := authedUser(t, svc)
	pid := seedProject(t, svc, name)
	st.ActiveProjectID = pid
	SaveSession(svc.deps.DB, st)
	return st
}

func insertCall(t *testing.T, pdb *db.ProjectDB, model, stage, calls, errMsg string, in, out int) {
	t.Helper()
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_llm_calls
		(source, stage, model, provider_name, input_tokens, output_tokens, duration_ms, response_content, response_tool_calls, response_stop_reason, error_message, request_tools, total_request_bytes)
		VALUES ('engine', ?, ?, 'Prov', ?, ?, 4200, 'Here is my <reply>', ?, 'tool_use', ?, '["manage_files","manage_data"]', 20480)`,
		stage, model, in, out, calls, errVal); err != nil {
		t.Fatal(err)
	}
}

func lastEdit(fp *fakeProvider) string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.edits) == 0 {
		return ""
	}
	return fp.edits[len(fp.edits)-1].text
}

func TestDashboardShowsProjectAndQuestions(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	screenUser(t, svc, "Acme")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'Brand colour?', 'pending', 'text')")

	svc.HandleCallback("telegram", "555", "999", 100, "nav:menu")

	text := lastEdit(fp)
	for _, want := range []string{"Chatting with", "Acme", "1 question(s) waiting", "Idea"} {
		if !strings.Contains(text, want) {
			t.Errorf("dashboard missing %q in %q", want, text)
		}
	}
	sc := svc.dashboardScreen(svc.getOrCreateUser("telegram", "555"))
	joined := strings.Join(sc.labels, "|")
	if !strings.Contains(joined, "Questions (1)") {
		t.Errorf("questions button should carry the count, got %q", joined)
	}
}

func TestStatusScreenReportsUsage(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	screenUser(t, svc, "Acme")
	insertCall(t, pdb, "m1", "build", "", "", 1000, 500)
	insertCall(t, pdb, "m1", "build", "", "boom", 0, 0)
	pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES ('t1','Do login','x','done',1), ('t2','Do pages','x','active',2)`)

	svc.HandleCallback("telegram", "555", "999", 100, "nav:status")

	text := lastEdit(fp)
	for _, want := range []string{"Acme", "1/2 tasks", "Do pages", "2 calls", "1 failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("status missing %q in:\n%s", want, text)
		}
	}
}

func TestQuestionsScreenAndDefer(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	screenUser(t, svc, "Acme")
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'First?', 'pending', 'text'), (2, 'Second?', 'pending', 'text')")
	answered := make(chan events.Event, 4)
	svc.deps.Bus.Subscribe(events.EventQuestionAnswered, func(e events.Event) { answered <- e })

	svc.HandleCallback("telegram", "555", "999", 100, "nav:questions")
	if text := lastEdit(fp); !strings.Contains(text, "First?") || !strings.Contains(text, "Second?") {
		t.Fatalf("questions list = %q", text)
	}

	// Opening a question delivers it with a Later button.
	svc.HandleCallback("telegram", "555", "999", 100, "q:open:1")
	fp.mu.Lock()
	delivered := strings.Join(fp.questions, "|")
	fp.mu.Unlock()
	if !strings.Contains(delivered, "First?") {
		t.Fatalf("question not delivered: %q", delivered)
	}

	// Later records the sentinel answer, marks it deferred and wakes the engine.
	svc.HandleCallback("telegram", "555", "999", 100, "q:defer:1")
	var status, ans string
	pdb.QueryRow("SELECT status FROM of_questions WHERE id=1").Scan(&status)
	pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id=1").Scan(&ans)
	if status != "deferred" || ans != DeferredAnswerText {
		t.Errorf("status=%q answer=%q", status, ans)
	}
	select {
	case e := <-answered:
		if e.Payload["deferred"] != true {
			t.Errorf("event payload = %v, want deferred", e.Payload)
		}
	default:
		t.Error("no answered event for the deferral")
	}

	// A second tap on the same button changes nothing.
	svc.HandleCallback("telegram", "555", "999", 100, "q:defer:1")
	var n int
	pdb.QueryRow("SELECT COUNT(*) FROM of_answers WHERE question_id=1").Scan(&n)
	if n != 1 {
		t.Errorf("deferral recorded %d times, want once", n)
	}

	// Skip everything that is left.
	svc.HandleCallback("telegram", "555", "999", 100, "q:deferall")
	pdb.QueryRow("SELECT COUNT(*) FROM of_questions WHERE status='pending'").Scan(&n)
	if n != 0 {
		t.Errorf("%d questions still pending", n)
	}
	if text := lastEdit(fp); !strings.Contains(text, "Nothing is waiting") {
		t.Errorf("empty state = %q", text)
	}
}

func TestLLMLogListAndDetail(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	screenUser(t, svc, "Acme")
	for i := 0; i < 8; i++ {
		insertCall(t, pdb, "big-model", "build", `[{"id":"c","name":"manage_files","arguments":"{\"action\":\"save\"}"}]`, "", 12400, 800)
	}
	insertCall(t, pdb, "big-model", "plan", "", "rate limited", 0, 0)

	svc.HandleCallback("telegram", "555", "999", 100, "nav:log")
	list := lastEdit(fp)
	for _, want := range []string{"LLM calls", "1-6 of 9", "#9", "rate limited", "manage_files", "12.4k/800"} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %q in:\n%s", want, list)
		}
	}
	sc := svc.logListScreen(svc.getOrCreateUser("telegram", "555"), 0)
	if !strings.Contains(strings.Join(sc.cbs, "|"), "log:p:1") {
		t.Errorf("first page should link to the next: %v", sc.cbs)
	}

	svc.HandleCallback("telegram", "555", "999", 100, "log:p:1")
	if page2 := lastEdit(fp); !strings.Contains(page2, "7-9 of 9") {
		t.Errorf("second page = %q", page2)
	}

	svc.HandleCallback("telegram", "555", "999", 100, "log:d:3")
	detail := lastEdit(fp)
	for _, want := range []string{"Call #3", "big-model", "12.4k in / 800 out", "tool_use", "manage_files", "&lt;reply&gt;", "2 tools offered", "20 KB"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q in:\n%s", want, detail)
		}
	}
	if len(detail) > 3800 {
		t.Errorf("detail is %d chars, too close to the cap", len(detail))
	}

	svc.HandleCallback("telegram", "555", "999", 100, "log:d:9")
	if failed := lastEdit(fp); !strings.Contains(failed, "rate limited") || !strings.Contains(failed, "✗") {
		t.Errorf("failed call detail = %q", failed)
	}
}

func TestLLMLogFileExport(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	screenUser(t, svc, "Acme")
	insertCall(t, pdb, "m", "build", "", "", 10, 5)

	toast := svc.HandleCallback("telegram", "555", "999", 100, "log:f:1")
	if toast != "Sent" {
		t.Fatalf("toast = %q", toast)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.docs) != 1 || fp.docs[0] != "llm-call-1.json" {
		t.Errorf("documents sent = %v", fp.docs)
	}

	name, data, err := svc.callFile(pdb, 1)
	if err != nil || name != "llm-call-1.json" || !strings.Contains(string(data), `"response"`) || !strings.Contains(string(data), "Here is my <reply>") {
		t.Errorf("callFile = %q %v %s", name, err, data)
	}
}

func TestCommandsOpenScreens(t *testing.T) {
	svc, fp, _ := newTestService(t)
	screenUser(t, svc, "Acme")

	for cmd, want := range map[string]string{
		"/status": "Acme", "/questions": "Nothing is waiting", "/log": "LLM calls",
		"/site": "Address", "/help": "Help", "/alerts": "Alerts", "/menu": "Chatting with",
	} {
		before := func() int { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.questions) }()
		svc.HandleIncoming("telegram", "555", "999", 1, cmd)
		fp.mu.Lock()
		if len(fp.questions) != before+1 || !strings.Contains(fp.questions[len(fp.questions)-1], want) {
			t.Errorf("%s: want a screen containing %q, got %v", cmd, want, fp.questions[before:])
		}
		fp.mu.Unlock()
	}
}

func TestAlertsChangeVerbosityAndPersist(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")

	svc.HandleCallback("telegram", "555", "999", 100, "verb:quiet")
	if st.verbosity() != VerbosityQuiet {
		t.Fatalf("verbosity = %q", st.verbosity())
	}
	loaded := LoadSessions(svc.deps.DB)["telegram:555"]
	if loaded == nil || loaded.verbosity() != VerbosityQuiet {
		t.Errorf("verbosity was not persisted: %+v", loaded)
	}
	svc.HandleCallback("telegram", "555", "999", 100, "verb:bogus")
	if st.verbosity() != VerbosityNormal {
		t.Errorf("unknown level should fall back to normal, got %q", st.verbosity())
	}
}

func TestGoLiveScreenLinks(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")

	local := svc.siteScreen(st)
	if strings.Contains(strings.Join(local.cbs, "|"), "url:") {
		t.Error("a project with no domain must not offer a link")
	}
	if !strings.Contains(local.text, "Set a domain") {
		t.Errorf("text = %q", local.text)
	}

	svc.deps.DB.DB.Exec("UPDATE projects SET domain='shop.example.com', published=1 WHERE id=?", st.ActiveProjectID)
	svc.deps.DB.SetSetting("admin_base_url", "https://admin.example.com/")
	pub := svc.siteScreen(st)
	joined := strings.Join(pub.cbs, "|")
	if !strings.Contains(joined, "url:https://shop.example.com") || !strings.Contains(joined, "url:https://admin.example.com/projects/") {
		t.Errorf("public project should link to site and admin, got %v", pub.cbs)
	}

	svc.deps.DB.DB.Exec("UPDATE projects SET domain='calm-fox.localhost' WHERE id=?", st.ActiveProjectID)
	loc := svc.siteScreen(st)
	if strings.Contains(strings.Join(loc.cbs, "|"), "url:https://calm-fox") || !strings.Contains(loc.text, "only works on the computer") {
		t.Errorf("a .localhost address must not be linked, got %v / %q", loc.cbs, loc.text)
	}
}

func TestURLButtonsBuildLinkKeyboards(t *testing.T) {
	kb, err := buildInlineKeyboard([]string{"Open", "Back"}, []string{"url:https://example.com/a?b=" + strings.Repeat("x", 100), "nav:menu"})
	if err != nil {
		t.Fatalf("long URLs are fine, callback data is what is capped: %v", err)
	}
	rows := kb["inline_keyboard"].([][]map[string]string)
	if rows[0][0]["url"] == "" || rows[0][0]["callback_data"] != "" {
		t.Errorf("url button = %v", rows[0][0])
	}
	if rows[0][1]["callback_data"] != "nav:menu" {
		t.Errorf("callback button = %v", rows[0][1])
	}
}
