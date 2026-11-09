package bot

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/db/models"
	"openfabrica/events"
)

func TestMarkdownBecomesTelegramHTML(t *testing.T) {
	cases := map[string]string{
		"**Your project is live!**":                     "<b>Your project is live!</b>",
		"Use `npm start` now":                           "Use <code>npm start</code> now",
		"- one\n- two":                                  "• one\n• two",
		"## Heading":                                    "<b>Heading</b>",
		"[Docs](https://example.com/a?x=1&y=2)":         `<a href="https://example.com/a?x=1&amp;y=2">Docs</a>`,
		"[Open Settings](/projects/1/settings-sharing)": "Open Settings",
		"a <script>alert(1)</script> & b":               "a &lt;script&gt;alert(1)&lt;/script&gt; &amp; b",
		"```js\nlet a = **1**;\n```":                    "<pre>let a = **1**;</pre>",
		"`**not bold**` and **bold**":                   "<code>**not bold**</code> and <b>bold</b>",
		"text\n\n\n\nmore":                              "text\n\nmore",
		"keep <!-- hidden --> this":                     "keep  this",
		"[x](javascript:alert(1))":                      "x",
		"[a](https://e.com/\"onclick=\"x)":              "a",
	}
	for in, want := range cases {
		if got := mdToTelegramHTML(in); got != want {
			t.Errorf("mdToTelegramHTML(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

func goLiveMessage(t *testing.T, info map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(info)
	return "**Your project is live!**\n\n- **Preview locally:** [http://localhost:5733](http://localhost:5733)\n\n<!--of-golive:" +
		base64.StdEncoding.EncodeToString(raw) + "-->"
}

func TestGoLiveMessageBecomesACard(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := screenUser(t, svc, "Shop")
	svc.deps.DB.DB.Exec("UPDATE projects SET domain='shop.example.com' WHERE id=?", st.ActiveProjectID)
	svc.subscribe()

	msg := goLiveMessage(t, map[string]any{
		"kind": "golive", "preview_url": "http://localhost:5733",
		"pages": []string{"home", "menu", "contact"}, "tables": []string{"orders"},
		"login": map[string]any{"minted": true, "label": "Email", "identifier": "demo@shop.test", "password": "s3cret<>"},
	})
	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineMessage, st.ActiveProjectID, map[string]any{"content": msg}))

	waitFor(t, func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.cards) > 0 })
	fp.mu.Lock()
	defer fp.mu.Unlock()
	card := fp.cards[0]
	for _, want := range []string{"Your project is live", "home, menu, contact", "orders", "demo@shop.test", "s3cret&lt;&gt;", "https://shop.example.com"} {
		if !strings.Contains(card, want) {
			t.Errorf("go-live card missing %q in:\n%s", want, card)
		}
	}
	for _, bad := range []string{"of-golive", "localhost", "**"} {
		if strings.Contains(card, bad) {
			t.Errorf("go-live card must not contain %q:\n%s", bad, card)
		}
	}
	for _, m := range fp.messages {
		if strings.Contains(m, "of-golive") || strings.Contains(m, "base64") {
			t.Errorf("raw payload leaked into a message: %q", m)
		}
	}
}

func TestGoLiveCardHasNoLinkForLocalDomain(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Shop")
	svc.deps.DB.DB.Exec("UPDATE projects SET domain='calm-fox.localhost' WHERE id=?", st.ActiveProjectID)
	project, _ := models.GetProjectByID(svc.deps.DB.DB, st.ActiveProjectID)

	sc := svc.goLiveCard(voice{}, project, &goLiveInfo{})
	if strings.Contains(strings.Join(sc.cbs, "|"), "url:") || !strings.Contains(sc.text, "only works on the computer") {
		t.Errorf("local project card = %q %v", sc.text, sc.cbs)
	}
}

func TestPlainMessageBecomesNormalChat(t *testing.T) {
	svc, fp, _ := newTestService(t)
	svc.subscribe()
	svc.HandleIncoming("telegram", "555", "999", 1, "hello builder") // stranger: not allowed
	fp.mu.Lock()
	n := len(fp.messages)
	fp.mu.Unlock()
	if n != 1 {
		t.Fatalf("stranger got %d replies", n)
	}
}

func TestCancelButtonReallyCancelsNewProject(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := authedUser(t, svc)
	seedProject(t, svc, "Old One")
	seedProject(t, svc, "Old Two")

	svc.HandleCallback("telegram", "555", "999", 10, "nav:new")
	if st.PendingAction != "awaiting_description" {
		t.Fatalf("pending = %q", st.PendingAction)
	}
	svc.HandleCallback("telegram", "555", "999", 10, "nav:menu") // the Cancel button
	if st.PendingAction != "" {
		t.Fatalf("Cancel left %q armed", st.PendingAction)
	}

	// The next ordinary message must not become a project.
	before := func() int { c := 0; ps, _ := models.ListProjects(svc.deps.DB.DB); c = len(ps); return c }()
	svc.HandleIncoming("telegram", "555", "999", 11, "hello there")
	after, _ := models.ListProjects(svc.deps.DB.DB)
	if len(after) != before {
		t.Errorf("a stray message created a project (%d -> %d)", before, len(after))
	}
	_ = fp
}

func TestSlashCommandAbandonsPendingPrompt(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := authedUser(t, svc)
	svc.HandleCallback("telegram", "555", "999", 10, "nav:new")
	svc.HandleIncoming("telegram", "555", "999", 11, "/help")
	if st.PendingAction != "" {
		t.Errorf("/help left %q armed", st.PendingAction)
	}
}

func TestFirstMessageOffersNewProject(t *testing.T) {
	svc, fp, _ := newTestService(t)
	seedDefaultModel(t, svc)
	st := authedUser(t, svc)

	svc.HandleIncoming("telegram", "555", "999", 1, "A website for my bakery")
	fp.mu.Lock()
	offer := ""
	if n := len(fp.questions); n > 0 {
		offer = fp.questions[n-1]
	}
	fp.mu.Unlock()
	if !strings.Contains(offer, "bakery") || !strings.Contains(offer, "Start a new one") {
		t.Fatalf("offer = %q", offer)
	}
	if st.PendingAction != "confirm_new" {
		t.Fatalf("pending = %q", st.PendingAction)
	}

	// A second message replaces the idea instead of being lost.
	svc.HandleIncoming("telegram", "555", "999", 2, "Actually a flower shop")
	if st.PendingData["idea"] != "Actually a flower shop" {
		t.Errorf("idea = %q", st.PendingData["idea"])
	}

	svc.HandleCallback("telegram", "555", "999", 3, "new:go")
	projects, _ := models.ListProjects(svc.deps.DB.DB)
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(projects))
	}
	p := projects[0]
	if st.ActiveProjectID != p.ID {
		t.Error("the new project should be active")
	}
	if p.Description == nil || *p.Description != "Actually a flower shop" {
		t.Errorf("brief = %v", p.Description)
	}
	if p.Domain == nil || !strings.HasSuffix(*p.Domain, ".localhost") {
		t.Errorf("domain = %v, want a minted .localhost address", p.Domain)
	}
	if strings.Contains(p.Name, "flower") || p.Name == "" {
		t.Errorf("name = %q, want a friendly minted name, not the sentence", p.Name)
	}
	if !models.ProjectNameIsAutoGenerated(&p) {
		t.Error("the minted name must be flagged so the engine can retitle it")
	}
	if svc.deps.ProjectDBManager.Get(p.ID) == nil {
		t.Error("project database not created")
	}

	// The offer cannot be replayed.
	if toast := svc.HandleCallback("telegram", "555", "999", 3, "new:go"); !strings.Contains(toast, "expired") {
		t.Errorf("replayed offer toast = %q", toast)
	}
}

func TestFirstMessageWithOneProjectJustUsesIt(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := authedUser(t, svc)
	pid := seedProject(t, svc, "Only One")

	svc.HandleIncoming("telegram", "555", "999", 1, "add a contact page")
	if st.ActiveProjectID != pid {
		t.Errorf("ActiveProjectID = %d, want the only project %d", st.ActiveProjectID, pid)
	}
	if st.PendingAction != "" {
		t.Errorf("pending = %q", st.PendingAction)
	}
}

func TestUnsupportedMessagesGetAnAnswer(t *testing.T) {
	svc, fp, _ := newTestService(t)
	authedUser(t, svc)
	tp := NewTelegramProvider("tok", svc.HandleIncoming, svc.HandleCallback, nil)
	tp.SetUnsupported(svc.replyUnsupported)

	photo := tgUpdate{Message: &tgMessage{MessageID: 3, Chat: tgChat{ID: 555, Type: "private"}, From: &tgUser{ID: 999}}}
	tp.dispatch(photo)
	fp.mu.Lock()
	got := strings.Join(fp.messages, "|")
	fp.mu.Unlock()
	if !strings.Contains(got, "only read text") {
		t.Errorf("no answer to a photo: %q", got)
	}

	// Strangers get nothing, so the bot does not confirm it exists.
	fp.mu.Lock()
	fp.messages = nil
	fp.mu.Unlock()
	tp.dispatch(tgUpdate{Message: &tgMessage{MessageID: 4, Chat: tgChat{ID: 1, Type: "private"}, From: &tgUser{ID: 1}}})
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.messages) != 0 {
		t.Errorf("stranger got %v", fp.messages)
	}
}

func TestOtherProjectsAlertYouWhenTheyNeedYou(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := authedUser(t, svc)
	a := seedProject(t, svc, "Alpha")
	b := seedProject(t, svc, "Beta")
	_ = pdb
	st.ActiveProjectID = a
	SaveSession(svc.deps.DB, st)

	svc.alertOthers(b, "question", "❓ <b>%s</b> has a question for you.")
	svc.alertOthers(b, "question", "❓ <b>%s</b> has a question for you.") // burst: only one

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.questions) != 1 || !strings.Contains(fp.questions[0], "Beta") {
		t.Errorf("alerts = %v, want exactly one about Beta", fp.questions)
	}

	// Someone already looking at that project is not alerted twice.
	st.ActiveProjectID = b
	svc.alertOthers(b, "error", "⚠️ <b>%s</b> ran into a problem.")
	if len(fp.questions) != 1 {
		t.Errorf("a person viewing the project was alerted: %v", fp.questions)
	}
}

func TestRenameKeepsMultibyteNamesIntact(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Old")
	long := strings.Repeat("é", 100)
	st.PendingAction = "awaiting_rename"
	svc.HandleIncoming("telegram", "555", "999", 5, long)

	p, _ := models.GetProjectByID(svc.deps.DB.DB, st.ActiveProjectID)
	if !strings.HasPrefix(p.Name, "é") || len([]rune(p.Name)) != 60 {
		t.Errorf("name = %q (%d runes), want 60 whole characters", p.Name, len([]rune(p.Name)))
	}
}

// seedDefaultModel makes project creation possible: a selectable default model.
func seedDefaultModel(t *testing.T, svc *BotService) {
	t.Helper()
	db := svc.deps.DB.DB
	if _, err := db.Exec(`INSERT INTO providers (name, vendor, transport, kind, is_enabled, requires_api_key)
		VALUES ('Test', 'test', 'openai-chat', 'llm', 1, 0)`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO llm_models (provider_id, model_id, display_name, is_enabled, is_default)
		VALUES ((SELECT id FROM providers WHERE name='Test'), 'test-model', 'Test Model', 1, 1)`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
}
