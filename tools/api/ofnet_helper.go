package api

import (
	_ "embed"

	"openfabrica/internal/dbh"
	"openfabrica/store"
)

// ofnetHelperJS is the platform-owned realtime client helper, emitted as a
// global-scope platform_managed file (js/ofnet.js) whenever a project creates a
// WebSocket endpoint. It wraps the /api/<path>/ws relay contract the model would
// otherwise re-implement by hand every build: presence roster seeding, message
// dispatch, direct send, and server-side matchmaking, where a "matched" frame
// auto-joins the server-assigned room so a page never derives a room id or runs
// pairing logic. An ES module (resolved via the importmap as the bare specifier
// "ofnet"); exposes named exports (connect, roomCode, clientId) plus a default
// namespace.
//
// The source is a real .js file so it lints, diffs, and highlights like the
// JavaScript it is. Keep it dependency-free: it loads standalone in the browser.
//
//go:embed js/ofnet.min.js
var ofnetHelperJS string

// emitRealtimeClientHelper writes js/ofnet.js into the project as a global-scope,
// platform-managed ES module (resolved via the importmap as the bare specifier
// "ofnet", and protected from model edit/delete). Best-effort: a save failure
// leaves the app to use the raw relay contract documented in the create result,
// so it never blocks endpoint creation. Idempotent via ArtifactSave's upsert.
func emitRealtimeClientHelper(db dbh.Handle) bool {
	a := &store.Artifact{
		Kind:        "file",
		Path:        "js/ofnet.js",
		Scope:       "global",
		ContentType: "application/javascript",
		Body:        ofnetHelperJS,
		Metadata:    map[string]any{"platform_managed": true},
	}
	if _, _, err := store.ArtifactSave(db, a, "platform:ofnet"); err != nil {
		return false
	}
	return true
}
