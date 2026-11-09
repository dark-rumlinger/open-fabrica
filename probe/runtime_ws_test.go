package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeRelay accepts a WebSocket and behaves per a keyword in the path so the
// smoke pass can be exercised against every outcome it must catch:
//   - "missing"  -> reject the upgrade (404), simulating an unmounted route.
//   - "silent"   -> upgrade then send nothing, the broken-relay case.
//   - "nowelcome"-> send a non-welcome first frame.
//   - "notoken"  -> welcome WITHOUT _joinToken (broken room gate).
//   - otherwise  -> a healthy welcome carrying a _joinToken.
func fakeRelay(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "no such endpoint", http.StatusNotFound)
			return
		}
		// "authreq" simulates a requires_auth endpoint: a tokenless handshake is
		// refused 401 before the upgrade, exactly like the real relay.
		if strings.Contains(r.URL.Path, "authreq") && r.URL.Query().Get("token") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		switch {
		case strings.Contains(r.URL.Path, "silent"):
			time.Sleep(200 * time.Millisecond) // upgrade, then never speak
		case strings.Contains(r.URL.Path, "nowelcome"):
			_ = wsjson.Write(ctx, c, map[string]any{"_type": "join"})
		case strings.Contains(r.URL.Path, "notoken"):
			_ = wsjson.Write(ctx, c, map[string]any{"_type": "welcome", "_clientId": "a"})
		default:
			_ = wsjson.Write(ctx, c, map[string]any{"_type": "welcome", "_clientId": "a", "_joinToken": "tok-123"})
		}
		time.Sleep(50 * time.Millisecond)
	}))
}

func TestWebSocketSmokePass(t *testing.T) {
	srv := fakeRelay(t)
	defer srv.Close()
	wsBase := strings.Replace(srv.URL, "http://", "ws://", 1)

	cases := []struct {
		name     string
		ep       wsEndpoint
		token    string
		wantFail bool
		wantMsg  string
	}{
		{"healthy ungated", wsEndpoint{path: "chat", roomGated: false}, "", false, ""},
		{"healthy gated with token", wsEndpoint{path: "callgated", roomGated: true}, "", false, ""},
		{"gated without token", wsEndpoint{path: "callnotoken", roomGated: true}, "", true, "no _joinToken"},
		{"upgrades but silent", wsEndpoint{path: "silent", roomGated: false}, "", true, "never emitted a welcome"},
		{"non-welcome first frame", wsEndpoint{path: "nowelcome", roomGated: false}, "", true, "not a welcome frame"},
		{"unmounted route", wsEndpoint{path: "missing", roomGated: false}, "", true, "failed its handshake"},
		// requires_auth: dialed WITH a token completes the handshake (no false 401).
		{"auth with token passes", wsEndpoint{path: "authreq", requiresAuth: true}, "t-abc", false, ""},
		// requires_auth but no usable token: skip rather than emit a false failure.
		{"auth no token skips", wsEndpoint{path: "authreq", requiresAuth: true}, "", false, ""},
		// tenant-scoped needs a tenant token the probe can't mint: skip.
		{"auth tenant-scoped skips", wsEndpoint{path: "authreq", requiresAuth: true, tenantColumn: "tenant_id"}, "t-abc", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss := wsSmokeOne(context.Background(), wsBase, "", tc.ep, tc.token)
			if tc.wantFail {
				if iss == nil {
					t.Fatalf("expected a failure issue, got nil")
				}
				if iss.Severity != "error" || iss.Category != "websocket" {
					t.Errorf("issue severity/category = %q/%q, want error/websocket", iss.Severity, iss.Category)
				}
				if tc.wantMsg != "" && !strings.Contains(iss.Message, tc.wantMsg) {
					t.Errorf("message %q does not mention %q", iss.Message, tc.wantMsg)
				}
			} else if iss != nil {
				t.Fatalf("expected pass, got issue: %s", iss.Message)
			}
		})
	}
}

// TestWSSmokeSignatureNamesTheMode pins that each welcome-smoke failure mode
// carries a distinct, non-empty Signature. Without it the sequential failure
// modes (handshake -> welcome -> first-frame -> join-token) share one
// Fingerprint() on a page, so clearing the first and revealing the next looks
// like "the same finding survived" and demotes it to a known issue.
func TestWSSmokeSignatureNamesTheMode(t *testing.T) {
	srv := fakeRelay(t)
	defer srv.Close()
	wsBase := strings.Replace(srv.URL, "http://", "ws://", 1)

	sig := func(ep wsEndpoint, token string) string {
		iss := wsSmokeOne(context.Background(), wsBase, "", ep, token)
		if iss == nil {
			t.Fatalf("expected a failure issue for %q", ep.path)
		}
		if iss.Signature == "" {
			t.Fatalf("every ws-smoke failure must carry a non-empty Signature (%q)", ep.path)
		}
		return iss.Signature
	}
	modes := []string{
		sig(wsEndpoint{path: "missing"}, ""),
		sig(wsEndpoint{path: "silent"}, ""),
		sig(wsEndpoint{path: "nowelcome"}, ""),
		sig(wsEndpoint{path: "notoken", roomGated: true}, ""),
	}
	seen := map[string]bool{}
	for _, s := range modes {
		if seen[s] {
			t.Fatalf("ws-smoke failure modes must carry distinct Signatures, got a duplicate %q in %v", s, modes)
		}
		seen[s] = true
	}
}

// TestWSSmokeAndReducerSmokeDoNotCollide pins the fingerprint split: one
// authoritative endpoint is inspected by BOTH the welcome-frame smoke (this file)
// and the reducer smoke (runtime_actor.go). Both emit Category "websocket", Page
// "/api/<path>/ws", File "" - only the per-source Signature keeps two distinct
// defects from collapsing to one Fingerprint() and demoting each other in the fix
// loop, which would ship a fixable break unshown.
func TestWSSmokeAndReducerSmokeDoNotCollide(t *testing.T) {
	srv := fakeRelay(t)
	defer srv.Close()
	wsBase := strings.Replace(srv.URL, "http://", "ws://", 1)

	// A welcome-smoke failure (upgrades then stays silent) on endpoint "silent"...
	wsIssue := wsSmokeOne(context.Background(), wsBase, "", wsEndpoint{path: "silent"}, "")
	if wsIssue == nil {
		t.Fatal("expected a welcome-smoke failure issue")
	}
	// ...and a reducer-smoke failure on the SAME logical endpoint path.
	redIssue := smokeReducer(authEndpoint{path: "silent",
		reducer: `function reduce(s,e,c){ if(e.type==='@init'){ throw new Error('boom'); } return s; }`})
	if redIssue == nil {
		t.Fatal("expected a reducer-smoke failure issue")
	}
	// Precondition: they share the coarse coordinates that caused the collision.
	if wsIssue.Category != redIssue.Category || wsIssue.Page != redIssue.Page || wsIssue.File != redIssue.File {
		t.Fatalf("precondition: the two smokes must share category+page+file: ws=%q/%q/%q reducer=%q/%q/%q",
			wsIssue.Category, wsIssue.Page, wsIssue.File, redIssue.Category, redIssue.Page, redIssue.File)
	}
	// Guarantee: distinct fingerprints, so the fix loop seeds and tracks them apart.
	if wsIssue.Fingerprint() == redIssue.Fingerprint() {
		t.Fatalf("ws-smoke and reducer-smoke on one endpoint must not share a fingerprint:\n ws=%q\n reducer=%q",
			wsIssue.Fingerprint(), redIssue.Fingerprint())
	}
}
