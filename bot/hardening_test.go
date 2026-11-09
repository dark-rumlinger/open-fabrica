package bot

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"openfabrica/events"
)

func TestSplitMessageNeverCutsAMultibyteCharacter(t *testing.T) {
	// 3-byte runes with no spaces or newlines to prefer, so the cut must land mid-rune
	// unless it is moved.
	text := strings.Repeat("日本語", 400)
	chunks := splitMessage(text, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected the text to split, got %d chunk(s)", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
		if len(c) > 100 {
			t.Errorf("chunk %d is %d bytes, over the 100 limit", i, len(c))
		}
	}
	if strings.Join(chunks, "") != text {
		t.Error("splitting lost or reordered text")
	}
}

// A delete confirmation is bound to the project it was shown for.
func TestDeleteConfirmationIsBoundToItsProject(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := armedUser(t, svc) // active project 1

	// A button built for project 7, tapped while project 1 is active: refused.
	if got := svc.handleConfirm(st, 0, "delete:7"); got != "Expired" {
		t.Errorf("stale confirmation answered %q, want Expired", got)
	}
	// An old unbound button is refused too.
	if got := svc.handleConfirm(st, 0, "delete"); got != "Expired" {
		t.Errorf("unbound confirmation answered %q, want Expired", got)
	}
	if st.ActiveProjectID != 1 {
		t.Error("a refused confirmation must leave the active project alone")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.messages) == 0 {
		t.Error("the user should be told why nothing happened")
	}
}

func TestQueuedHandlersRunInOrderOffThePublisher(t *testing.T) {
	svc, _, _ := newTestService(t)
	var got []int
	done := make(chan struct{})
	h := svc.queued(func(e events.Event) {
		got = append(got, e.ProjectID)
		if len(got) == 3 {
			close(done)
		}
	})
	started := time.Now()
	for i := 1; i <= 3; i++ {
		h(events.Event{ProjectID: i})
	}
	if time.Since(started) > time.Second {
		t.Error("queuing must not block the publisher")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued handlers never ran")
	}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("order = %v, want 1 2 3", got)
	}
}

func TestOptionQuestionTextIsEscapedForHTML(t *testing.T) {
	svc, fp, _ := newTestService(t)
	st := armedUser(t, svc)
	svc.deliverQuestion(st, "1", "Is 2 < 3 & <b>bold</b>?", []string{"Yes", "No"}, "single_choice", "", "")

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.questions) == 0 {
		t.Fatal("no question was sent")
	}
	got := fp.questions[len(fp.questions)-1]
	if strings.Contains(got, "<b>") || !strings.Contains(got, "&lt;b&gt;") || !strings.Contains(got, "&amp;") {
		t.Errorf("question text was not escaped: %q", got)
	}
}
