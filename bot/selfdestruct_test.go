package bot

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// armSelfDestruct installs a recording erase hook and walks to the final screen.
func armSelfDestruct(t *testing.T, svc *BotService, st *UserState) (nonceCB string, ran *atomic.Bool, exited chan struct{}) {
	t.Helper()
	ran = &atomic.Bool{}
	exited = make(chan struct{})
	svc.SetSelfDestruct(func() SelfDestructReport {
		ran.Store(true)
		return SelfDestructReport{}
	}, func() { close(exited) })

	sc := svc.selfDestructFinalScreen(st)
	for _, cb := range sc.cbs {
		if strings.HasPrefix(cb, "cfm:sd:") {
			return cb, ran, exited
		}
	}
	t.Fatalf("final screen has no confirm button: %+v", sc.cbs)
	return "", nil, nil
}

func TestSelfDestructUnavailableWithoutHook(t *testing.T) {
	svc, _, _ := newTestService(t)
	sc := svc.selfDestructWarnScreen(voice{})
	for _, cb := range sc.cbs {
		if strings.HasPrefix(cb, "nav:selfdestruct2") {
			t.Fatal("warning screen offers to continue without an erase hook")
		}
	}
}

func TestSelfDestructRunsOnValidToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := armedUser(t, svc)
	cb, ran, exited := armSelfDestruct(t, svc, st)

	if len(cb) > tgMaxCallbackLen {
		t.Fatalf("callback data is %d bytes, over the %d byte limit", len(cb), tgMaxCallbackLen)
	}
	svc.HandleCallback("telegram", "555", "999", 7, cb)

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process exit was not requested")
	}
	if !ran.Load() {
		t.Error("erase did not run")
	}
}

func TestSelfDestructTokenIsSingleUse(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := armedUser(t, svc)
	cb, ran, exited := armSelfDestruct(t, svc, st)

	svc.HandleCallback("telegram", "555", "999", 7, cb)
	<-exited
	ran.Store(false)

	if got := svc.HandleCallback("telegram", "555", "999", 7, cb); got != "Expired" {
		t.Errorf("replayed token result = %q, want Expired", got)
	}
	if ran.Load() {
		t.Error("a replayed token ran the erase again")
	}
}

func TestSelfDestructRejectsExpiredAndWrongChat(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := armedUser(t, svc)
	cb, ran, _ := armSelfDestruct(t, svc, st)

	svc.sd.mu.Lock()
	svc.sd.expires = time.Now().Add(-time.Second)
	svc.sd.mu.Unlock()
	if got := svc.HandleCallback("telegram", "555", "999", 7, cb); got != "Expired" {
		t.Errorf("expired token result = %q, want Expired", got)
	}

	cb, _, _ = armSelfDestruct(t, svc, st)
	other := svc.getOrCreateUser("telegram", "777")
	other.UserID = "999"
	if got := svc.HandleCallback("telegram", "777", "999", 7, cb); got != "Expired" {
		t.Errorf("token from another chat result = %q, want Expired", got)
	}
	if ran.Load() {
		t.Error("erase ran for a rejected token")
	}
}

func TestSelfDestructRejectsForgedToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := armedUser(t, svc)
	_, ran, _ := armSelfDestruct(t, svc, st)

	for _, forged := range []string{"cfm:sd:", "cfm:sd:0000000000000000", "cfm:sd"} {
		svc.HandleCallback("telegram", "555", "999", 7, forged)
	}
	if ran.Load() {
		t.Error("erase ran for a forged token")
	}
}

func TestSelfDestructStrangerIsIgnored(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := armedUser(t, svc)
	cb, ran, _ := armSelfDestruct(t, svc, st)

	svc.HandleCallback("telegram", "555", "123456", 7, cb)
	if ran.Load() {
		t.Error("a user outside the allowlist triggered the erase")
	}
}
