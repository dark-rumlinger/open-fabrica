package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"openfabrica/db/models"
	"openfabrica/engine/enginetest"
	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/replylang"
	"openfabrica/providers/llm"
)

// firstRequestAfterWake runs one LIVE owner-chat wake on a project whose reply
// language is directive and returns the first request the model received.
func firstRequestAfterWake(t *testing.T, directive string) llm.CompletionRequest {
	t.Helper()
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("ok")}}
	w, _ := scheduledTestWorker(t, p)
	if directive != "" {
		if err := models.SetProjectReplyLanguage(w.deps.DB.DB, 1, directive); err != nil {
			t.Fatalf("set reply language: %v", err)
		}
	}
	w.handleChatWake(context.Background(), "any ideas for the homepage?")
	if len(p.Requests) == 0 {
		t.Fatal("the wake made no model request")
	}
	return p.Requests[0]
}

func joinedMessages(req llm.CompletionRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// The reply language must never reach the system prompt. That prompt is shared by
// every project and is the long cached prefix, so a language in it would give each
// language its own cache and break the byte-stability the golden prompt test pins.
func TestReplyLanguageNeverTouchesTheSystemPrompt(t *testing.T) {
	base := firstRequestAfterWake(t, "")
	if strings.Contains(joinedMessages(base), "Reply language") {
		t.Fatal("a project with no reply language must send no language instruction")
	}
	for _, directive := range []string{"es", "ar", "zh", replylang.Match} {
		req := firstRequestAfterWake(t, directive)
		if req.System != base.System {
			t.Errorf("directive %q changed the system prompt", directive)
		}
		if strings.Contains(req.System, "Reply language") {
			t.Errorf("directive %q leaked into the system prompt", directive)
		}
		if len(req.Tools) != len(base.Tools) {
			t.Errorf("directive %q changed the tool set: %d vs %d", directive, len(req.Tools), len(base.Tools))
		}
	}
}

func TestReplyLanguageLandsInTheOwnerContextMessage(t *testing.T) {
	for _, directive := range []string{"es", "ja", replylang.Match} {
		req := firstRequestAfterWake(t, directive)
		want := replylang.Line(directive)
		found := false
		for _, m := range req.Messages {
			if strings.Contains(m.Content, ownerContextSentinel) {
				found = strings.Contains(m.Content, want)
				break
			}
		}
		if !found {
			t.Errorf("directive %q: the owner context message does not carry %q", directive, want)
		}
	}
}

func TestOwnerContextLanguageLine(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)

	if got := w.ownerContext(); got != "" {
		t.Fatalf("no owner and no language should render nothing, got %q", got)
	}

	if _, err := w.deps.DB.ExecWrite(
		`INSERT INTO users (username, password_hash, display_name, role) VALUES ('ada', 'x', 'Ada', 'admin')`); err != nil {
		t.Fatal(err)
	}
	if got, want := w.ownerContext(), "## Context\n- Owner: Ada\n\n"; got != want {
		t.Fatalf("without a language the context must be unchanged:\n got %q\nwant %q", got, want)
	}

	if err := models.SetProjectReplyLanguage(w.deps.DB.DB, 1, "fr"); err != nil {
		t.Fatal(err)
	}
	want := "## Context\n- Owner: Ada\n" + replylang.Line("fr") + "\n\n"
	if got := w.ownerContext(); got != want {
		t.Fatalf("with a language:\n got %q\nwant %q", got, want)
	}
}

// The worker reads the language from the database each time. A value cached on the
// worker would make a restarted worker, or a reseed after a rollover, write a
// different prompt than the one it replaces.
func TestReplyLanguageIsReadFromTheDatabaseEveryTime(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	for _, directive := range []string{"de", replylang.Match, "", "pt"} {
		if err := models.SetProjectReplyLanguage(w.deps.DB.DB, 1, directive); err != nil {
			t.Fatal(err)
		}
		if got := w.replyLanguage(); got != directive {
			t.Errorf("replyLanguage() = %q after storing %q", got, directive)
		}
	}
}

func TestReplyLanguageWithoutDepsIsEmpty(t *testing.T) {
	if got := (&Worker{}).replyLanguage(); got != "" {
		t.Errorf("a worker with no deps must report no language, got %q", got)
	}
}

// A message that reaches a running build carries the current language itself,
// because the build's opening context may predate a change.
func TestOwnerInboxNoteCarriesTheCurrentLanguage(t *testing.T) {
	plain := ownerInboxNote([]string{"hola", "gracias"}, "")
	if want := ownerDuringBuildTag + " hola\n" + ownerDuringBuildTag + " gracias"; plain != want {
		t.Errorf("no language must leave the note unchanged:\n got %q\nwant %q", plain, want)
	}
	withLang := ownerInboxNote([]string{"hola", "gracias"}, "es")
	if !strings.HasPrefix(withLang, plain+"\n") {
		t.Errorf("the language must follow the messages: %q", withLang)
	}
	if strings.Count(withLang, replylang.Reminder("es")) != 1 {
		t.Errorf("the reminder must appear exactly once: %q", withLang)
	}
	if ownerInboxNote(nil, "es") != "" {
		t.Error("no messages must produce no note, whatever the language")
	}
}

func TestMidBuildOwnerMessageGetsTheReminder(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	if err := models.SetProjectReplyLanguage(w.deps.DB.DB, 1, "ja"); err != nil {
		t.Fatal(err)
	}
	w.pushOwnerInbox("ヘッダーを青くして")
	hook := w.withOwnerInbox(func(int, []llm.ToolCall, int) (string, *ending) { return "", nil })
	inject, _ := hook(0, nil, 0)
	if !strings.Contains(inject, "ヘッダーを青くして") || !strings.Contains(inject, replylang.Reminder("ja")) {
		t.Errorf("the injected turn must carry the message and the reminder, got %q", inject)
	}
}

// publishedBy runs fn on a worker whose project replies in lang and returns the
// chat lines it saved, oldest first, and the engine.error payloads it published.
func publishedBy(t *testing.T, lang string, fn func(w *Worker)) (lines []string, errorEvents []string) {
	t.Helper()
	w, pdb := scheduledTestWorker(t, nil)
	w.deps.Bus = events.NewBus()
	w.deps.Bus.Subscribe(events.EventEngineError, func(e events.Event) {
		s, _ := e.Payload["error"].(string)
		errorEvents = append(errorEvents, s)
	})
	if lang != "" {
		if err := models.SetProjectReplyLanguage(w.deps.DB.DB, 1, lang); err != nil {
			t.Fatal(err)
		}
	}
	fn(w)
	rows, err := pdb.Reader().Query(`SELECT content FROM of_chat_messages WHERE session_id = 'brain' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lines, errorEvents
}

const stoppedLine = "Stopped there. Tell me what you'd like instead."

func TestPublishMessageSpeaksTheProjectsLanguage(t *testing.T) {
	want := msgcat.Catalog("es")[stoppedLine]
	if want == "" {
		t.Fatal("the Spanish catalog lacks the stop line")
	}
	lines, _ := publishedBy(t, "es", func(w *Worker) { w.publishMessage(stoppedLine) })
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("got %q, want %q", lines, want)
	}
}

func TestPublishMessageTranslatesTemplatedLines(t *testing.T) {
	lines, _ := publishedBy(t, "de", func(w *Worker) {
		w.publishMessage(fmt.Sprintf("That didn't go through the first time (try %d of %d). Giving it another go now.", 2, 3))
	})
	want := "Das hat beim ersten Mal nicht geklappt (Versuch 2 von 3). Ich probiere es jetzt noch einmal."
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("got %q, want %q", lines, want)
	}
}

func TestPublishMessageLeavesEnglishAndUnknownTextAlone(t *testing.T) {
	model := "Here are three ideas for the homepage: a hero banner, a menu, and a map."
	for _, lang := range []string{"", replylang.Match, "en", "xx"} {
		lines, _ := publishedBy(t, lang, func(w *Worker) {
			w.publishMessage(stoppedLine)
			w.publishMessage(model)
		})
		if len(lines) != 2 || lines[0] != stoppedLine || lines[1] != model {
			t.Errorf("lang %q changed text: %q", lang, lines)
		}
	}
	// A language with a catalog still leaves text a model wrote untouched.
	lines, _ := publishedBy(t, "es", func(w *Worker) { w.publishMessage(model) })
	if len(lines) != 1 || lines[0] != model {
		t.Errorf("model text was altered: %q", lines)
	}
}

func TestPublishVerbatimNeverTranslates(t *testing.T) {
	lines, _ := publishedBy(t, "es", func(w *Worker) { w.publishVerbatim(stoppedLine) })
	if len(lines) != 1 || lines[0] != stoppedLine {
		t.Errorf("got %q", lines)
	}
}

func TestPublishErrorTranslatesTheLineAndTheEvent(t *testing.T) {
	cat := msgcat.Catalog("fr")
	line := "I kept running into trouble during build (3 tries) and paused so nothing breaks. Latest issue: provider timeout. Let me know and we'll sort it together."
	lines, events := publishedBy(t, "fr", func(w *Worker) {
		w.publishError(line)
	})
	inner := msgcat.Translate("fr", line)
	if inner == line {
		t.Fatal("the error line was not translated")
	}
	prefix := strings.TrimSuffix(cat["**Pipeline Error:** %s"], "%s")
	if len(lines) != 1 || lines[0] != prefix+inner {
		t.Errorf("chat line %q, want %q", lines, prefix+inner)
	}
	if len(events) != 1 || events[0] != inner {
		t.Errorf("event %q, want %q", events, inner)
	}
}

// A composite of a model's sentence and one of our own lines translates our part
// and leaves the model's alone.
func TestTitleAnnouncementTranslatesOnlyItsOwnLine(t *testing.T) {
	var got string
	publishedBy(t, "es", func(w *Worker) {
		// No model is wired, so the reaction fails and the fallback line is used.
		got = w.titleAnnouncement(context.Background(), "a bakery site", "Sunrise Bakery")
	})
	if !strings.Contains(got, "Sunrise Bakery") {
		t.Fatalf("title missing from %q", got)
	}
	for _, english := range []string{"Love it", "it is", "Going with"} {
		if strings.Contains(got, english) {
			t.Errorf("announcement still has English %q: %q", english, got)
		}
	}
}

func TestEveryStageStartLineIsTranslated(t *testing.T) {
	for _, stage := range []string{StageExpand, StagePlan, StageBuild} {
		for _, line := range stageInfo(stage).StartLines {
			for _, lang := range msgcat.Languages() {
				if msgcat.Translate(lang, line) == line {
					t.Errorf("%s start line %q has no %s translation", stage, line, lang)
				}
			}
		}
	}
}

func TestRefusedGoLiveSpeaksTheProjectsLanguageAroundTheFindings(t *testing.T) {
	issues := []Issue{{Page: "index.html", Message: "Button does nothing. Repro detail follows."}}
	lines, _ := publishedBy(t, "de", func(w *Worker) { w.refuseGoLive(issues) })
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	for _, want := range []string{"**Ich habe vor dem Deployment angehalten.**", "ein Problem", "**index.html**: Button does nothing", "Sag mir, womit ich anfangen soll"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("missing %q in %q", want, lines[0])
		}
	}
	if strings.Contains(lines[0], "I stopped") || strings.Contains(lines[0], "Repro detail") {
		t.Errorf("English leaked, or the finding was not trimmed to its first sentence: %q", lines[0])
	}

	lines, _ = publishedBy(t, "", func(w *Worker) { w.refuseGoLive(issues) })
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "**I stopped before deploying this.**") {
		t.Errorf("English changed: %q", lines)
	}
}
