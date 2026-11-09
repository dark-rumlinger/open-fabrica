package bot

import (
	"strings"
	"testing"
	"time"

	"openfabrica/events"
)

func TestStartPayload(t *testing.T) {
	cases := map[string]string{
		"/start ABC":          "ABC",
		"/start@my_bot ABC":   "ABC",
		"/START abc":          "abc",
		"/start":              "",
		"/menu ABC":           "",
		"hello ABC":           "",
		"/start ABC and more": "",
	}
	for in, want := range cases {
		if got := startPayload(in); got != want {
			t.Errorf("startPayload(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPairingAddsUserAndPublishesEvent(t *testing.T) {
	svc, fp, _ := newTestService(t)
	paired := make(chan events.Event, 1)
	svc.deps.Bus.Subscribe(events.EventBotPaired, func(e events.Event) { paired <- e })

	code, _, err := svc.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	svc.noteUser("777", "Mark")
	svc.HandleIncoming("telegram", "777", "777", 1, "/start "+code)

	if !svc.isAllowed("777") {
		t.Fatal("user was not added to the allowlist")
	}
	if got := svc.Users(); len(got) != 1 || got[0].Name != "Mark" {
		t.Errorf("Users() = %+v, want one user named Mark", got)
	}
	select {
	case <-paired:
	default:
		t.Error("EventBotPaired was not published")
	}
	fp.mu.Lock()
	sent := strings.Join(fp.messages, "\n")
	fp.mu.Unlock()
	if !strings.Contains(sent, "connected") {
		t.Errorf("welcome not sent, got %q", sent)
	}

	// The code is single use.
	svc.HandleIncoming("telegram", "888", "888", 2, "/start "+code)
	if svc.isAllowed("888") {
		t.Error("a used code must not admit a second person")
	}
}

func TestPairingRejectsBadAndExpiredCodes(t *testing.T) {
	svc, _, _ := newTestService(t)
	code, _, _ := svc.NewPairingCode()

	svc.HandleIncoming("telegram", "10", "10", 1, "/start WRONGCODE")
	if svc.isAllowed("10") {
		t.Fatal("bad code admitted a user")
	}

	svc.pair.mu.Lock()
	c := svc.pair.codes[code]
	c.expires = time.Now().Add(-time.Second)
	svc.pair.codes[code] = c
	svc.pair.mu.Unlock()
	svc.HandleIncoming("telegram", "11", "11", 2, "/start "+code)
	if svc.isAllowed("11") {
		t.Error("expired code admitted a user")
	}
}

func TestPairingFailureLimit(t *testing.T) {
	svc, fp, _ := newTestService(t)
	for i := 0; i < pairFailLimit+3; i++ {
		svc.HandleIncoming("telegram", "50", "50", i, "/start NOPE")
	}
	// The valid code arrives after the chat has been rate limited.
	code, _, _ := svc.NewPairingCode()
	svc.HandleIncoming("telegram", "50", "50", 99, "/start "+code)
	if svc.isAllowed("50") {
		t.Error("a limited chat must stay locked out for the window")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.messages) > pairFailLimit {
		t.Errorf("bot answered %d times, want at most %d", len(fp.messages), pairFailLimit)
	}
}

func TestUnauthorizedRepliesAreThrottled(t *testing.T) {
	svc, fp, _ := newTestService(t)
	for i := 0; i < 40; i++ {
		svc.HandleIncoming("telegram", "60", "60", i, "hello")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.messages) > int(rlBurst) {
		t.Errorf("bot replied %d times to a stranger, want at most %d", len(fp.messages), int(rlBurst))
	}
}

func TestAddAndRemoveUsers(t *testing.T) {
	svc, _, _ := newTestService(t)
	if err := svc.AddUser("1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddUser("2", "Bo"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddUser("2", "Bo"); err != nil {
		t.Fatal(err)
	}
	users := svc.Users()
	if len(users) != 2 || users[0].Name != "Ann" || users[1].Name != "Bo" {
		t.Fatalf("users = %+v, want Ann and Bo once each", users)
	}
	if err := svc.RemoveUser("1"); err != nil {
		t.Fatal(err)
	}
	if svc.isAllowed("1") || !svc.isAllowed("2") {
		t.Error("removal state wrong")
	}
	if err := svc.RemoveUser("1"); err == nil {
		t.Error("removing an unknown user should fail")
	}
}
