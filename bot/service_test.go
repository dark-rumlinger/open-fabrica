package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/security"
)

// fakeProvider is an in-memory Provider that records what the bot would send,
// so BotService flows can be driven without touching Telegram.
type fakeProvider struct {
	mu        sync.Mutex
	messages  []string
	questions []string
	cards     []string // SendCard texts
	docs      []string // SendDocument filenames
	deleted   []int
	actions   []string
	edits     []editRecord // EditMessageKeyboard calls
	keyboards []keyboard   // SendQuestion buttons
	nextID    int
	failQ     bool // make SendQuestion fail (simulate no-button fallback)
	failEdit  bool // make EditMessageKeyboard fail (simulate stale message)
}

type editRecord struct {
	id     int
	text   string
	markup string // reply_markup as JSON
}

type keyboard struct {
	labels []string
	cbs    []string
}

func (f *fakeProvider) Name() string                    { return "telegram" }
func (f *fakeProvider) Start(ctx context.Context) error { return nil }
func (f *fakeProvider) Stop() error                     { return nil }

func (f *fakeProvider) SendMessage(chatID, text string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.messages = append(f.messages, text)
	return f.nextID, nil
}

func (f *fakeProvider) SendQuestion(chatID, question string, labels, cbs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQ {
		return fmt.Errorf("no buttons")
	}
	f.questions = append(f.questions, question)
	f.keyboards = append(f.keyboards, keyboard{labels: labels, cbs: cbs})
	return nil
}

func (f *fakeProvider) EditMessage(chatID string, id int, text string) error { return nil }

func (f *fakeProvider) EditMessageKeyboard(chatID string, id int, text string, replyMarkup map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEdit {
		return fmt.Errorf("message to edit not found")
	}
	raw, _ := json.Marshal(replyMarkup)
	f.edits = append(f.edits, editRecord{id: id, text: text, markup: string(raw)})
	return nil
}

func (f *fakeProvider) DeleteMessage(chatID string, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeProvider) SendCard(chatID, text string, replyMarkup map[string]any, silent bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.cards = append(f.cards, text)
	return f.nextID, nil
}

func (f *fakeProvider) SendDocument(chatID, filename string, data []byte, caption string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, filename)
	return nil
}

func (f *fakeProvider) SendChatAction(chatID, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)
	return nil
}

func newTestService(t *testing.T) (*BotService, *fakeProvider, *db.ProjectDB) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })

	pm := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(func() { pm.CloseAll() })
	pdb, err := pm.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}

	enc, err := security.NewEncryptor("0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	svc := NewBotService(&BotDeps{
		DB:               gdb,
		ProjectDBManager: pm,
		Bus:              events.NewBus(),
		Encryptor:        enc,
		Logger:           slog.Default(),
	})
	fp := &fakeProvider{}
	svc.RegisterProvider(fp)
	return svc, fp, pdb
}

// armedUser returns a user state authorized and pointed at the test project.
func armedUser(t *testing.T, svc *BotService) *UserState {
	t.Helper()
	if err := svc.deps.DB.SetSetting("telegram_allowed_users", "999"); err != nil {
		t.Fatalf("set allowed users: %v", err)
	}
	st := svc.getOrCreateUser("telegram", "555")
	st.UserID = "999"
	st.ActiveProjectID = 1
	SaveSession(svc.deps.DB, st)
	return st
}

// TestFreeTextAnswerRecorded covers the free-text answer path: an option-less question
// answered by typing (not a button) must be recorded and resume the build.
func TestFreeTextAnswerRecorded(t *testing.T) {
	svc, _, pdb := newTestService(t)
	st := armedUser(t, svc)
	st.PendingAction = "awaiting_answer"
	st.PendingData = map[string]string{"question_id": "1"}
	SaveSession(svc.deps.DB, st)

	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type) VALUES (1, 'Brand name?', 'pending', 'text')")

	answered := make(chan events.Event, 1)
	svc.deps.Bus.Subscribe(events.EventQuestionAnswered, func(e events.Event) { answered <- e })

	svc.HandleIncoming("telegram", "555", "999", 42, "Acme Co")

	var ans, status string
	if err := pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id=1").Scan(&ans); err != nil {
		t.Fatalf("answer not recorded: %v", err)
	}
	if ans != "Acme Co" {
		t.Errorf("answer = %q, want %q", ans, "Acme Co")
	}
	pdb.QueryRow("SELECT status FROM of_questions WHERE id=1").Scan(&status)
	if status != "answered" {
		t.Errorf("status = %q, want answered", status)
	}
	select {
	case <-answered:
	default:
		t.Error("EventQuestionAnswered was not published")
	}
}

// TestFormQuestionAnswered covers multi-field "form" questions: each field is
// answered in sequence, a text field lands in of_memory, a secret field is
// encrypted into of_secrets (and the pasted message scrubbed), and the question
// is finalized with a summary, matching the admin's form handling.
func TestFormQuestionAnswered(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)

	fieldsJSON := `[{"name":"api_key","label":"API Key","type":"text"},{"name":"stripe","label":"Stripe Secret","type":"secret","secret_name":"STRIPE_KEY"}]`
	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type, fields) VALUES (1, 'Configure Stripe', 'pending', 'form', ?)", fieldsJSON)

	answered := make(chan events.Event, 1)
	svc.deps.Bus.Subscribe(events.EventQuestionAnswered, func(e events.Event) { answered <- e })

	// Deliver the question -> arms the form flow, prompts field 1.
	st.Mu.Lock()
	svc.promptQuestionByID(st, "1")
	st.Mu.Unlock()
	if st.PendingAction != "awaiting_form" {
		t.Fatalf("PendingAction = %q, want awaiting_form", st.PendingAction)
	}

	// Field 1 (text) -> of_memory.
	svc.HandleIncoming("telegram", "555", "999", 20, "abc123")
	var mem string
	if err := pdb.QueryRow("SELECT value FROM of_memory WHERE key='api_key'").Scan(&mem); err != nil || mem != "abc123" {
		t.Fatalf("of_memory api_key = %q (err %v), want abc123", mem, err)
	}

	// Field 2 (secret) -> of_secrets (encrypted) + message scrubbed + finalize.
	svc.HandleIncoming("telegram", "555", "999", 21, "sk_live_xyz")
	var enc string
	if err := pdb.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name='STRIPE_KEY'").Scan(&enc); err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if dec, err := svc.deps.Encryptor.Decrypt(enc); err != nil || dec != "sk_live_xyz" {
		t.Errorf("decrypt = %q (err %v), want the secret value", dec, err)
	}

	var status, summary string
	pdb.QueryRow("SELECT status FROM of_questions WHERE id=1").Scan(&status)
	if status != "answered" {
		t.Errorf("question status = %q, want answered", status)
	}
	pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id=1").Scan(&summary)
	if summary != "Configured: API Key, Stripe Secret" {
		t.Errorf("summary = %q, want 'Configured: API Key, Stripe Secret'", summary)
	}

	fp.mu.Lock()
	scrubbed := false
	for _, id := range fp.deleted {
		if id == 21 {
			scrubbed = true
		}
	}
	fp.mu.Unlock()
	if !scrubbed {
		t.Errorf("secret message (21) was not scrubbed: deleted=%v", fp.deleted)
	}

	select {
	case <-answered:
	default:
		t.Error("EventQuestionAnswered was not published")
	}
}

// TestAnswerIdempotent covers the double-tap / redelivery case: answering the
// same question twice must record exactly one answer.
func TestAnswerIdempotent(t *testing.T) {
	svc, _, pdb := newTestService(t)
	st := armedUser(t, svc)
	pdb.ExecWrite(`INSERT INTO of_questions (id, question, status, options) VALUES (1, 'Pick', 'pending', '["A","B"]')`)

	st.Mu.Lock()
	svc.answerQuestionByIndex(st, "1", "0")
	svc.answerQuestionByIndex(st, "1", "0") // duplicate
	st.Mu.Unlock()

	var count int
	pdb.QueryRow("SELECT COUNT(*) FROM of_answers WHERE question_id=1").Scan(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 answer after double-tap, got %d", count)
	}
}

// TestSecretAnswerStoredAndScrubbed covers secret handling: a secret answer is encrypted
// into of_secrets, of_answers holds only a redaction, and the pasted message is
// deleted from the chat.
func TestSecretAnswerStoredAndScrubbed(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	st := armedUser(t, svc)
	st.PendingAction = "awaiting_answer"
	SaveSession(svc.deps.DB, st)

	pdb.ExecWrite("INSERT INTO of_questions (id, question, status, type, secret_name) VALUES (1, 'API key?', 'pending', 'secret', 'STRIPE_KEY')")

	svc.HandleIncoming("telegram", "555", "999", 77, "sk_live_supersecretvalue")

	var enc string
	if err := pdb.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name='STRIPE_KEY'").Scan(&enc); err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if dec, err := svc.deps.Encryptor.Decrypt(enc); err != nil || dec != "sk_live_supersecretvalue" {
		t.Errorf("decrypt = %q (err %v), want the secret value", dec, err)
	}

	var ans string
	pdb.QueryRow("SELECT answer FROM of_answers WHERE question_id=1").Scan(&ans)
	if ans != "(secret provided)" {
		t.Errorf("of_answers = %q; the raw secret may have leaked", ans)
	}

	fp.mu.Lock()
	deleted := append([]int(nil), fp.deleted...)
	fp.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != 77 {
		t.Errorf("pasted secret message not scrubbed: deleted=%v", deleted)
	}
}

func TestIsAllowedParsesCSV(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.deps.DB.SetSetting("telegram_allowed_users", " 111 , 222,333 ")
	for _, id := range []string{"111", "222", "333"} {
		if !svc.isAllowed(id) {
			t.Errorf("%q should be allowed", id)
		}
	}
	for _, id := range []string{"", "11", "3330", "999"} {
		if svc.isAllowed(id) {
			t.Errorf("%q should be denied", id)
		}
	}
}

func TestRateLimiterEnforcesBurst(t *testing.T) {
	svc, _, _ := newTestService(t)
	allowed := 0
	for i := 0; i < 30; i++ {
		if svc.allowRate("u1") {
			allowed++
		}
	}
	if allowed < 1 || allowed > int(rlBurst)+1 {
		t.Errorf("allowed %d requests, want between 1 and %d", allowed, int(rlBurst)+1)
	}
}

func TestOffsetRoundTrip(t *testing.T) {
	svc, _, _ := newTestService(t)
	if svc.LoadOffset() != 0 {
		t.Error("default offset should be 0")
	}
	svc.SaveOffset(12345)
	if got := svc.LoadOffset(); got != 12345 {
		t.Errorf("offset round-trip = %d, want 12345", got)
	}
}
