// Package replayid gives a user-flow replay (of_test.js) its own identity for rate
// limiting. Endpoint limits are keyed by client IP, and every replay comes from the
// same headless browser on loopback, so all its flows, all its simulated users and
// every replay in the last minute share ONE bucket that a real visitor would have to
// themselves. A suite of a few flows then trips an app's 60-per-minute limit and
// ships a 429 the app would never serve a person - and the model "fixes" it by
// raising the limit.
//
// A replay registers an unguessable token for as long as it runs; each session tab
// sends it in Cookie together with the flow and session it belongs to. The server
// counts such a request against that simulated visitor instead of the shared IP, but
// only while the token is active, so the cookie is worthless outside a running
// replay and cannot be used to dodge a limit.
package replayid

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
)

// Cookie carries "<token>:<visitor>" from a replay's session tabs. It is a cookie
// scoped to the project's origin, never a request header: CDP's extra headers ride
// EVERY request the tab makes, cross-origin ones included, and a custom header turns
// each CDN module fetch (import * as THREE from 'three') into a CORS preflight the CDN
// refuses - the page's whole module graph then dies in the replay while it boots fine
// for a visitor. A cookie reaches only the project and never triggers a preflight.
const Cookie = "of_flow_replay"

var (
	mu     sync.Mutex
	active = map[string]bool{}
)

// Begin registers a fresh token for one replay and returns it with the func that
// retires it. Callers defer end so a finished replay's cookie stops counting.
func Begin() (token string, end func()) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)
	mu.Lock()
	active[token] = true
	mu.Unlock()
	return token, func() {
		mu.Lock()
		delete(active, token)
		mu.Unlock()
	}
}

// Value is the cookie value a session sends: the token plus the simulated visitor
// (flow and session), so each visitor gets a bucket of its own.
func Value(token, visitor string) string {
	return token + ":" + visitor
}

// Identity returns the rate-limit identity for a cookie value carrying an active
// token ("replay:<token>:<visitor>"), or "" when the value is absent, malformed or
// its replay has ended.
func Identity(value string) string {
	token, visitor, ok := strings.Cut(value, ":")
	if !ok || token == "" || visitor == "" {
		return ""
	}
	mu.Lock()
	on := active[token]
	mu.Unlock()
	if !on {
		return ""
	}
	return "replay:" + token + ":" + visitor
}
