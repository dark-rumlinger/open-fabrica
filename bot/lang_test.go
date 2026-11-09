package bot

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/buildstate"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/projectphase"
)

func TestUnknownLanguageStaysEnglish(t *testing.T) {
	for _, lang := range []string{"", "xx", "en"} {
		if got := (voice{lang: lang}).tr("Home"); got != "Home" {
			t.Errorf("lang %q: Home = %q, want English", lang, got)
		}
	}
	if got := (voice{lang: "de"}).tr("Home"); got != "Start" {
		t.Errorf("de Home = %q", got)
	}
}

func TestTrhEscapesArgumentsButNotNumbers(t *testing.T) {
	v := voice{lang: "de"}
	got := v.trh("Frage: %s", "<b>x</b>")
	if got != "Frage: &lt;b&gt;x&lt;/b&gt;" {
		t.Errorf("escape = %q", got)
	}
	got = v.trh("<b>Question %d of %d</b>", 2, 5)
	if got != "<b>Frage 2 von 5</b>" {
		t.Errorf("numbers = %q", got)
	}
}

func TestScreenFollowsTelegramLanguage(t *testing.T) {
	svc, fp, _ := newTestService(t)
	screenUser(t, svc, "Acme")
	svc.noteLanguage("999", "de-DE")

	svc.HandleCallback("telegram", "555", "999", 100, "nav:menu")
	text := lastEdit(fp)
	if !strings.Contains(text, "Chat mit") || !strings.Contains(text, "Acme") {
		t.Errorf("dashboard not in German: %q", text)
	}
	if strings.Contains(text, "Chatting with") {
		t.Errorf("English leaked into the German dashboard: %q", text)
	}

	svc.HandleCallback("telegram", "555", "999", 100, "nav:help")
	if help := lastEdit(fp); !strings.Contains(help, "Hilfe") || !strings.Contains(help, "/selfdestruct") {
		t.Errorf("help not in German with commands intact: %q", help)
	}
}

func TestToastsAreTranslated(t *testing.T) {
	svc, _, _ := newTestService(t)
	authedUser(t, svc)
	svc.noteLanguage("999", "fr")
	if got := svc.HandleCallback("telegram", "555", "999", 100, "nav:status"); got != "Choisissez d'abord un projet" {
		t.Errorf("toast = %q", got)
	}
}

func TestProjectReplyLanguageWinsOverTelegram(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")
	svc.noteLanguage("999", "de")
	if err := models.SetProjectReplyLanguage(svc.deps.DB.DB, st.ActiveProjectID, "fr"); err != nil {
		t.Fatal(err)
	}

	svc.HandleCallback("telegram", "555", "999", 100, "nav:menu")
	if text := lastEdit(fp); !strings.Contains(text, "Discussion avec") {
		t.Errorf("project language not used: %q", text)
	}

	// "match" and "" are not a language: the person's own app decides again.
	if err := models.SetProjectReplyLanguage(svc.deps.DB.DB, st.ActiveProjectID, "match"); err != nil {
		t.Fatal(err)
	}
	if got := svc.lang(st); got != "de" {
		t.Errorf("lang with match directive = %q, want de", got)
	}
}

func TestLanguageIsKeptOnlyForAllowedPeople(t *testing.T) {
	svc, _, _ := newTestService(t)
	authedUser(t, svc) // allows 999

	svc.noteLanguage("999", "es")
	svc.noteLanguage("777", "fr")

	if got := svc.userLang("999"); got != "es" {
		t.Errorf("allowed lang = %q", got)
	}
	if got := svc.userLang("777"); got != "fr" {
		t.Errorf("stranger lang = %q (memory)", got)
	}
	saved, _ := svc.deps.DB.GetSetting(userLangsKey)
	if !strings.Contains(saved, `"999":"es"`) || strings.Contains(saved, "777") {
		t.Errorf("saved languages = %q, want only the allowed person", saved)
	}

	again := NewBotService(svc.deps)
	if got := again.userLang("999"); got != "es" {
		t.Errorf("after restart = %q, want es", got)
	}
	if got := again.userLang("777"); got != "" {
		t.Errorf("stranger after restart = %q, want none", got)
	}

	// Garbage and unknown languages are ignored.
	svc.noteLanguage("999", "")
	svc.noteLanguage("999", "tlh")
	if got := svc.userLang("999"); got != "es" {
		t.Errorf("lang after junk = %q", got)
	}
}

func TestStrangerIsRefusedInTheirLanguage(t *testing.T) {
	svc, fp, _ := newTestService(t)
	svc.noteLanguage("777", "fr")
	svc.HandleIncoming("telegram", "888", "777", 1, "hello")

	fp.mu.Lock()
	sent := strings.Join(fp.messages, "\n")
	fp.mu.Unlock()
	if !strings.Contains(sent, "Ce bot est privé") || !strings.Contains(sent, "777") {
		t.Errorf("refusal = %q", sent)
	}
}

func TestPairingWelcomesInTheirLanguageAndKeepsIt(t *testing.T) {
	svc, fp, _ := newTestService(t)
	code, _, err := svc.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	svc.noteLanguage("777", "ja")
	svc.HandleIncoming("telegram", "777", "777", 1, "/start "+code)

	fp.mu.Lock()
	sent := strings.Join(fp.messages, "\n")
	fp.mu.Unlock()
	if !strings.Contains(sent, "接続しました") {
		t.Errorf("welcome not in Japanese: %q", sent)
	}
	saved, _ := svc.deps.DB.GetSetting(userLangsKey)
	if !strings.Contains(saved, `"777":"ja"`) {
		t.Errorf("language not kept after pairing: %q", saved)
	}
}

func TestBroadcastWordsEachRecipientInTheirLanguage(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st1 := screenUser(t, svc, "Acme")
	st2 := svc.getOrCreateUser("telegram", "556")
	st2.UserID = "998"
	st2.ActiveProjectID = st1.ActiveProjectID
	svc.noteLanguage("999", "es")
	svc.noteLanguage("998", "ru")

	svc.broadcastRender(st1.ActiveProjectID, func(v voice) string { return v.tr("Open Fabrica started.") })

	fp.mu.Lock()
	sent := strings.Join(fp.messages, "\n")
	fp.mu.Unlock()
	if !strings.Contains(sent, "Open Fabrica se ha iniciado.") || !strings.Contains(sent, "Open Fabrica запущен.") {
		t.Errorf("broadcast = %q", sent)
	}
}

func TestPhaseLineIsTranslated(t *testing.T) {
	paused := projectphase.Derive(projectphase.Inputs{Mode: models.ModePaused})
	if got := (voice{lang: "es"}).phaseLine(paused); got != "En pausa · Pausado por ti" {
		t.Errorf("es paused = %q", got)
	}
	needs := projectphase.Derive(projectphase.Inputs{
		Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused, PauseReason: buildstate.PauseAwaitingAnswers,
		Pending: 3, EngineRunning: true,
	})
	if got := (voice{lang: "de"}).phaseLine(needs); got != "Du wirst gebraucht" {
		t.Errorf("de needs you = %q", got)
	}
	if got := (voice{lang: "de"}).reasonLine(needs); got != "3 Fragen warten auf deine Antwort" {
		t.Errorf("de reason = %q", got)
	}
}

func TestToolLabelsAreTranslatedButActionsKept(t *testing.T) {
	if got := toolLabel("manage_files", map[string]any{"action": "list_files"}); got != "Managing files (list files)" {
		t.Errorf("english label = %q", got)
	}
	if got := (voice{lang: "de"}).toolLabel("manage_files", "list_files"); got != "Dateien werden verwaltet (list files)" {
		t.Errorf("de label = %q", got)
	}
	// A tool we have no label for keeps its own name.
	if got := (voice{lang: "de"}).toolLabel("manage_other", ""); got != "manage other" {
		t.Errorf("unknown tool = %q", got)
	}
	// A step written without a tool shows its label as is.
	if got := (voice{lang: "de"}).stepLabel(cardStep{label: "Managing data"}); got != "Managing data" {
		t.Errorf("label-only step = %q", got)
	}
}

func TestLiveCardViewTranslates(t *testing.T) {
	svc, _, _ := newTestService(t)
	text, labels, _ := svc.cardView(voice{lang: "ru"}, "App", cardState{
		thinking: true, total: 4, done: 1, stage: "BUILD",
		steps: []cardStep{{tool: "manage_data", label: "Managing data", status: "ok"}},
	}, false)
	for _, want := range []string{"Думаю", "задач: 1/4", "Сборка", "Управление данными"} {
		if !strings.Contains(text, want) {
			t.Errorf("card missing %q in %q", want, text)
		}
	}
	if len(labels) == 0 || labels[0] != "⏹ Стоп" {
		t.Errorf("labels = %v", labels)
	}
}

func TestTelegramReportsLanguage(t *testing.T) {
	tp := NewTelegramProvider("tok", func(string, string, string, int, string) {}, func(string, string, string, int, string) string { return "" }, nil)
	var gotID, gotLang string
	tp.SetLanguageSeen(func(id, lang string) { gotID, gotLang = id, lang })

	tp.dispatch(tgUpdate{Message: &tgMessage{
		MessageID: 1, Text: "hi", Chat: tgChat{ID: 42, Type: "private"},
		From: &tgUser{ID: 42, FirstName: "Dark", LanguageCode: "pt-br"},
	}})
	if gotID != "42" || gotLang != "pt-br" {
		t.Errorf("message language = %q %q", gotID, gotLang)
	}

	gotID, gotLang = "", ""
	tp.dispatch(tgUpdate{CallbackQuery: &tgCallbackQuery{
		ID: "c1", Data: "nav:menu", Message: &tgMessage{MessageID: 2, Chat: tgChat{ID: 7, Type: "private"}},
		From: &tgUser{ID: 7, LanguageCode: "ja"},
	}})
	if gotID != "7" || gotLang != "ja" {
		t.Errorf("callback language = %q %q", gotID, gotLang)
	}
}

func TestTgLang(t *testing.T) {
	cases := map[string]string{"pt-BR": "pt", "zh-hans": "zh", "DE": "de", "": "", "tlh": ""}
	for in, want := range cases {
		if got := tgLang(&tgUser{LanguageCode: in}); got != want {
			t.Errorf("tgLang(%q) = %q, want %q", in, got, want)
		}
	}
	if tgLang(nil) != "" {
		t.Error("nil user should be English")
	}
}

func TestGroupWarningIsInTheSendersLanguage(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()
	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second},
		logger: slog.Default(), warnedChats: map[int64]bool{}}

	if tp.ensurePrivate(tgChat{ID: -5, Type: "group"}, "de") {
		t.Fatal("a group must not be accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "Direktnachrichten") {
		t.Errorf("warning = %v", bodies)
	}
}

func TestProfileIsPublishedInEveryLanguage(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{} // method -> language codes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		lang := ""
		if i := strings.Index(string(b), `"language_code":"`); i >= 0 {
			rest := string(b)[i+len(`"language_code":"`):]
			lang = rest[:strings.Index(rest, `"`)]
		}
		mu.Lock()
		got[method] = append(got[method], lang)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()
	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, logger: slog.Default()}

	tp.setCommands()
	tp.setProfile()

	mu.Lock()
	defer mu.Unlock()
	want := len(msgcat.Languages()) + 1 // the default set plus one per catalog
	for _, method := range []string{"setMyCommands", "setMyDescription", "setMyShortDescription"} {
		if len(got[method]) != want {
			t.Errorf("%s called %d times, want %d: %v", method, len(got[method]), want, got[method])
		}
	}
	if len(got["setChatMenuButton"]) != 1 {
		t.Errorf("menu button set %d times", len(got["setChatMenuButton"]))
	}
}

func waitForMessage(t *testing.T, fp *fakeProvider, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fp.mu.Lock()
		sent := strings.Join(fp.messages, "\n")
		fp.mu.Unlock()
		if strings.Contains(sent, want) || time.Now().After(deadline) {
			return sent
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExpandedBriefHeadingIsInTheReadersLanguage(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")
	svc.noteLanguage("999", "de")

	svc.onBrainMessage(events.NewEvent(events.EventEngineMessage, st.ActiveProjectID, map[string]any{
		"content": engine.ExpandedBriefMarker + "\n\nA tip calculator with <split> support.",
	}))

	sent := waitForMessage(t, fp, "Erweiterte Beschreibung")
	if !strings.Contains(sent, "<b>Erweiterte Beschreibung</b>") || !strings.Contains(sent, "&lt;split&gt;") {
		t.Errorf("brief = %q", sent)
	}
	if strings.Contains(sent, "Expanded brief") || strings.Contains(sent, "**") {
		t.Errorf("English marker leaked: %q", sent)
	}
}

func TestExpandedBriefStaysEnglishWithoutALanguage(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")

	svc.onBrainMessage(events.NewEvent(events.EventEngineMessage, st.ActiveProjectID, map[string]any{
		"content": engine.ExpandedBriefMarker + "\n\nA tip calculator.",
	}))

	if sent := waitForMessage(t, fp, "tip calculator"); !strings.Contains(sent, "<b>Expanded brief</b>\n\nA tip calculator.") {
		t.Errorf("brief = %q", sent)
	}
}
