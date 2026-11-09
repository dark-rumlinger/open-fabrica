package api

import (
	_ "embed"

	"openfabrica/internal/dbh"
	"openfabrica/store"
)

// e2eeHelperJS is the platform-owned end-to-end encryption helper, emitted as a
// global-scope platform_managed file (js/e2ee.js) whenever a project creates or
// updates a WebSocket endpoint with e2ee=true. It is the single source of truth for
// the crypto: ECDH P-256 + PBKDF2(password) -> HKDF -> non-extractable AES-GCM-256,
// fresh-IV-per-message, AEAD sender/room binding, a replay counter namespaced per
// ephemeral key, fail-closed decrypt, and SHA-256 verify code. Shipping it from the
// platform keeps the guarantee real: a dropped AAD or weakened iteration count still
// encrypts between honest peers but is not secure, and the relay can only check the
// {iv,ct} envelope is present, never that it is well-formed (see
// server/public/ws_e2ee.go e2eeFrameAllowed). The wire envelope
// {type:'chat', v:1, iv, ct} is co-enforced by that relay, keeping client and server
// in lockstep.
//
// Two-party Session and multi-party GroupSession (pairwise-wrapped per-epoch key,
// re-minted on join/leave) share that envelope and enforcement; the only extra frame
// is the 'e2ee-gkey' control frame carrying the wrapped epoch key (no readable body,
// so it passes e2eeFrameAllowed). Each session also carries a persistent ECDSA P-256
// device identity (signs the ephemeral key so a swapped key is rejected; safetyNumber
// + trust-on-first-use), an optional per-message ratchet (ratchet:true, ephemeral
// rooms), and chunked blob transfer (encryptBlob/decryptBlob), all keyed off the same
// shared secret, browser-side. A reloading peer announces a fresh ephemeral key on the
// same client id, which both session modes treat as a re-handshake; the replay counter
// is keyed by that ephemeral pubkey (the group path's epoch), so a relay replaying a
// stale signed handshake re-derives the old key but its messages land on that key's old
// high-water mark and are dropped. Persistence has its own key: the live secret is
// per-connection and unrecoverable, so a room that stores history opens with
// archive:true, which keys messages on the password alone (readable in any later
// session, no forward secrecy, refused alongside ratchet:true). An ES module (bare
// specifier "e2ee"); exposes named exports (session, groupSession, identity) plus a
// default namespace.
//
// The source is a real .js file so it lints, diffs, and highlights like the
// JavaScript it is. Keep it dependency-free: it loads standalone in the browser.
//
//go:embed js/e2ee.min.js
var e2eeHelperJS string

// emitE2EEClientHelper writes js/e2ee.js into the project as a global-scope,
// platform-managed artifact (auto-injected into every page by the renderer, and
// protected from model edit/delete). Mirrors emitRealtimeClientHelper. Best-effort:
// a save failure leaves the app to follow the crypto contract documented in the
// guide, so it never blocks endpoint creation. Idempotent via ArtifactSave's upsert.
func emitE2EEClientHelper(db dbh.Handle) bool {
	a := &store.Artifact{
		Kind:        "file",
		Path:        "js/e2ee.js",
		Scope:       "global",
		ContentType: "application/javascript",
		Body:        e2eeHelperJS,
		Metadata:    map[string]any{"platform_managed": true},
	}
	if _, _, err := store.ArtifactSave(db, a, "platform:e2ee"); err != nil {
		return false
	}
	return true
}
