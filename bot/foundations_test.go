package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"openfabrica/events"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// A provider that appears after boot (token saved while the app runs) must still
// receive engine messages: the subscriptions can't depend on Start seeing one.
func TestReloadSubscribesWithoutStart(t *testing.T) {
	svc, fp, _ := newTestService(t)
	armedUser(t, svc)

	svc.ReloadTelegram(context.Background())

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineMessage, 1, map[string]any{"content": "hello there"}))

	waitFor(t, func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		for _, m := range fp.messages {
			if strings.Contains(m, "hello there") {
				return true
			}
		}
		return false
	})
}

func TestSubscribeIsIdempotent(t *testing.T) {
	svc, fp, _ := newTestService(t)
	armedUser(t, svc)

	svc.subscribe()
	svc.subscribe()
	svc.Start(context.Background())

	svc.deps.Bus.Publish(events.NewEvent(events.EventEngineMessage, 1, map[string]any{"content": "once"}))
	time.Sleep(200 * time.Millisecond)

	fp.mu.Lock()
	defer fp.mu.Unlock()
	n := 0
	for _, m := range fp.messages {
		if m == "once" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("message delivered %d times, want 1", n)
	}
}

func TestBotTokenEncryptedAtRest(t *testing.T) {
	svc, _, _ := newTestService(t)
	enc := svc.deps.Encryptor

	if err := SaveBotToken(svc.deps.DB, enc, "123:ABC"); err != nil {
		t.Fatal(err)
	}
	raw, _ := svc.deps.DB.GetSetting(TokenSettingKey)
	if strings.Contains(raw, "123:ABC") {
		t.Fatalf("token stored in plaintext: %q", raw)
	}
	if got := LoadBotToken(svc.deps.DB, enc); got != "123:ABC" {
		t.Errorf("round trip = %q", got)
	}

	// Legacy plaintext value is readable and gets upgraded.
	svc.deps.DB.SetSetting(TokenSettingKey, "999:ZZZ")
	if got := LoadBotToken(svc.deps.DB, enc); got != "999:ZZZ" {
		t.Errorf("legacy read = %q", got)
	}
	UpgradeBotToken(svc.deps.DB, enc)
	raw, _ = svc.deps.DB.GetSetting(TokenSettingKey)
	if !strings.HasPrefix(raw, tokenEncPrefix) {
		t.Errorf("legacy token not upgraded: %q", raw)
	}
	if got := LoadBotToken(svc.deps.DB, enc); got != "999:ZZZ" {
		t.Errorf("after upgrade = %q", got)
	}
}

func TestTelegramReportsWhoIsTalking(t *testing.T) {
	tp := NewTelegramProvider("tok", func(string, string, string, int, string) {}, func(string, string, string, int, string) string { return "" }, nil)
	var gotID, gotName string
	tp.SetUserSeen(func(id, name string) { gotID, gotName = id, name })

	tp.dispatch(tgUpdate{Message: &tgMessage{
		MessageID: 1, Text: "hi", Chat: tgChat{ID: 42, Type: "private"},
		From: &tgUser{ID: 42, FirstName: "Dark", LastName: "R"},
	}})
	if gotID != "42" || gotName != "Dark R" {
		t.Errorf("seen = %q %q", gotID, gotName)
	}
}

func TestDisconnectClearsHealth(t *testing.T) {
	svc, _, _ := newTestService(t)
	setBotHealth(true, "")
	botLastPollUnix.Store(123)

	// A real provider is registered, then its token is removed.
	svc.deps.DB.SetSetting(TokenSettingKey, "1:abc")
	tp := NewTelegramProvider("1:abc", svc.HandleIncoming, svc.HandleCallback, svc)
	svc.mu.Lock()
	svc.providers["telegram"] = tp
	svc.mu.Unlock()
	svc.deps.DB.SetSetting(TokenSettingKey, "")

	svc.ReloadTelegram(context.Background())
	if st := GetStats(); st.Healthy || st.LastPollUnix != 0 {
		t.Errorf("health after disconnect = %+v", st)
	}
	if _, ok := svc.provider("telegram"); ok {
		t.Error("provider should be removed")
	}
}
