package bot

import (
	"strings"
	"sync"
	"testing"
	"time"

	"openfabrica/events"
)

// fastCards shrinks the card timings for the duration of a test.
func fastCards(t *testing.T) {
	t.Helper()
	oldMin, oldIdle, oldRepost, oldTick := cardMinInterval.Load(), cardIdleAfter.Load(), cardRepostEvery.Load(), cardTick.Load()
	cardMinInterval.Store(20 * time.Millisecond)
	cardIdleAfter.Store(150 * time.Millisecond)
	cardRepostEvery.Store(0)
	cardTick.Store(30 * time.Millisecond)
	t.Cleanup(func() {
		cardMinInterval.Store(oldMin)
		cardIdleAfter.Store(oldIdle)
		cardRepostEvery.Store(oldRepost)
		cardTick.Store(oldTick)
	})
}

func lastCard(fp *fakeProvider) string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if n := len(fp.edits); n > 0 {
		return fp.edits[n-1].text
	}
	if n := len(fp.cards); n > 0 {
		return fp.cards[n-1]
	}
	return ""
}

func TestLiveCardFollowsEngineEvents(t *testing.T) {
	fastCards(t)
	svc, fp, _ := newTestService(t)
	armedUser(t, svc)
	svc.subscribe()
	bus := svc.deps.Bus

	bus.Publish(events.NewEvent(events.EventEngineStageChange, 1, map[string]any{"stage": "BUILD"}))
	bus.Publish(events.NewEvent(events.EventEngineTasks, 1, map[string]any{
		"total": 8, "done": 3,
		"tasks": []map[string]any{
			{"id": 1, "title": "Write the login page", "status": "active"},
			{"id": 2, "title": "Later", "status": "pending"},
		},
	}))
	bus.Publish(events.NewEvent(events.EventEngineToolStart, 1, map[string]any{
		"tool": "manage_files", "call_id": "c1", "args": map[string]any{"action": "save"},
	}))
	bus.Publish(events.NewEvent(events.EventEngineLLMCall, 1, map[string]any{
		"log_id": 7, "stage": "build", "model": "test-model", "input_tokens": 12400, "output_tokens": 812,
		"duration_ms": 8200, "cost_usd": 0.04,
	}))

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last card: %s", lastCard(fp))
		}
	})
	waitFor(t, func() bool {
		c := lastCard(fp)
		return strings.Contains(c, "3/8 tasks") && strings.Contains(c, "Write the login page") &&
			strings.Contains(c, "Managing files (save)") && strings.Contains(c, "test-model") &&
			strings.Contains(c, "$0.04") && strings.Contains(c, "Building")
	})

	// Finishing the tool flips its mark from running to done.
	bus.Publish(events.NewEvent(events.EventEngineToolResult, 1, map[string]any{"tool": "manage_files", "call_id": "c1"}))
	waitFor(t, func() bool { return strings.Contains(lastCard(fp), "✓ Managing files (save)") })

	// After a quiet spell the card settles into its idle form.
	waitFor(t, func() bool { return strings.Contains(lastCard(fp), "Waiting for you") })
}

func TestLiveCardEditsInPlace(t *testing.T) {
	fastCards(t)
	svc, fp, _ := newTestService(t)
	armedUser(t, svc)
	svc.subscribe()

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineThinking, 1, map[string]any{"state": "start"}))
	waitFor(t, func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.cards) == 1 })

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineTasks, 1, map[string]any{"total": 2, "done": 1}))
	waitFor(t, func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.edits) >= 1 })

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.cards) != 1 {
		t.Errorf("posted %d cards, want the first one edited in place", len(fp.cards))
	}
}

func TestLiveCardRepostsWhenPushedUp(t *testing.T) {
	fastCards(t)
	svc, fp, _ := newTestService(t)
	st := armedUser(t, svc)
	svc.subscribe()

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineThinking, 1, map[string]any{"state": "start"}))
	waitFor(t, func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.cards) == 1 })

	// A reply lands below the card, so the next update moves the card down.
	svc.broadcastToProjectUsers(1, "hello from the builder")
	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineTasks, 1, map[string]any{"total": 4, "done": 1}))

	waitFor(t, func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.cards) == 2 })
	fp.mu.Lock()
	deleted := len(fp.deleted)
	fp.mu.Unlock()
	if deleted == 0 {
		t.Error("the old card should be deleted when it is reposted")
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.LiveCardMessageID == 0 {
		t.Error("the new card id was not remembered")
	}
}

func TestQuietPeopleGetNoCard(t *testing.T) {
	fastCards(t)
	svc, fp, _ := newTestService(t)
	st := armedUser(t, svc)
	st.setVerbosity(VerbosityQuiet)
	svc.subscribe()

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineStageChange, 1, map[string]any{"stage": "PLAN"}))
	waitFor(t, func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		for _, m := range fp.messages {
			if strings.Contains(m, "Stage: PLAN") {
				return true
			}
		}
		return false
	})
	time.Sleep(100 * time.Millisecond)
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.cards) != 0 {
		t.Errorf("quiet person got %d cards", len(fp.cards))
	}
}

func TestNormalPeopleGetCardInsteadOfStageLine(t *testing.T) {
	fastCards(t)
	svc, fp, _ := newTestService(t)
	armedUser(t, svc)
	svc.subscribe()

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineStageChange, 1, map[string]any{"stage": "PLAN"}))
	waitFor(t, func() bool { return strings.Contains(lastCard(fp), "Mapping it out") })
	time.Sleep(100 * time.Millisecond)
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, m := range fp.messages {
		if strings.Contains(m, "Stage: PLAN") {
			t.Error("the card already shows the stage, no separate line expected")
		}
	}
}

func TestCardStateResetsForNewRun(t *testing.T) {
	fastCards(t)
	svc, _, _ := newTestService(t)
	armedUser(t, svc)
	svc.subscribe()

	var wg sync.WaitGroup
	wg.Add(1)
	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineLLMCall, 1, map[string]any{"model": "m", "input_tokens": 100, "output_tokens": 10}))
	go func() {
		defer wg.Done()
		waitFor(t, func() bool {
			c := svc.live.card(1)
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.st.idleShown
		})
	}()
	wg.Wait()

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineLLMCall, 1, map[string]any{"model": "m", "input_tokens": 5, "output_tokens": 1}))
	c := svc.live.card(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.calls != 1 || c.st.inTokens != 5 {
		t.Errorf("a new run should start from zero, got calls=%d in=%d", c.st.calls, c.st.inTokens)
	}
}

func TestCardViewFormatting(t *testing.T) {
	svc, _, _ := newTestService(t)
	text, labels, _ := svc.cardView(voice{}, "My <App>", cardState{
		stage: "BUILD", total: 10, done: 5, activeTask: "Do <b>it</b>", thinking: true,
		steps: []cardStep{{label: "Managing data", status: "ok"}, {label: "Running SQL", status: "fail"}},
		calls: 3, inTokens: 2500, outTokens: 500, cost: 0.5, costKnown: true, runStart: time.Now().Add(-75 * time.Second),
	}, false)

	for _, want := range []string{"My &lt;App&gt;", "▰▰▰▰▰▱▱▱▱▱ 5/10", "Do &lt;b&gt;it&lt;/b&gt;", "✓ Managing data", "✗ Running SQL", "Σ 3 calls · 3.0k tokens · $0.50 · 1m 15s", "Thinking"} {
		if !strings.Contains(text, want) {
			t.Errorf("card missing %q in:\n%s", want, text)
		}
	}
	if len(labels) != 4 || labels[0] != "⏹ Stop" {
		t.Errorf("active card buttons = %v", labels)
	}
	if len(text) > 3500 {
		t.Errorf("card is %d chars, too close to Telegram's cap", len(text))
	}

	_, paused, cbs := svc.cardView(voice{}, "x", cardState{paused: true}, true)
	if len(paused) == 0 || cbs[0] != "card:resume" {
		t.Errorf("paused card should offer Resume first, got %v %v", paused, cbs)
	}
}
