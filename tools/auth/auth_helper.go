package auth

import (
	_ "embed"
	"strings"

	"openfabrica/internal/dbh"
	"openfabrica/store"
)

// authBootstrapJS is the platform-owned browser auth bootstrap, emitted as a
// global-scope platform_managed ES module (js/auth.js, resolved via the importmap
// as the bare specifier "auth") whenever a project creates an auth endpoint. It is
// the SINGLE source of truth for the auth helpers every page imports (user,
// authReady, requireRole, requireAuth, redirectIfLoggedIn, apiFetch, logout). The
// guarantees baked in here (fragment drain runs at module eval, authReady is a
// single in-flight /me promise because the module evaluates once, a 401/403 drops
// the token while a network error does NOT) are the rules generated code tends to
// get wrong when a model authors this file per build, manifesting as the
// /admin <-> /login redirect-flash loop (why validateAuthBootstrapUsage and the
// late-globals gate exist). Owning the file removes that defect class at the
// source. The module is auto-injected on every page (so the bootstrap always
// runs) AND importable by any leaf; `user` is a live-binding export.
//
// __AUTH_PATH__ is substituted with the auth endpoint path at emit time (the only
// per-project variable). The source is a real .js file so it lints, diffs, and
// highlights like the JavaScript it is. Keep it dependency-free: it loads
// standalone in the browser.
//
//go:embed js/auth.min.js
var authBootstrapJS string

// emitAuthClientHelper writes js/auth.js into the project as a global-scope,
// platform-managed artifact (auto-injected into every page by the renderer, and
// protected from model edit/delete). Mirrors emitRealtimeClientHelper, with the
// one per-project substitution (__AUTH_PATH__ -> the auth endpoint path). The
// path is already normalized (no /api/ prefix, no leading slash) by the caller.
// Best-effort: a save failure leaves the auth_contract guide as the fallback, so
// it never blocks endpoint creation. Idempotent via ArtifactSave's upsert.
func emitAuthClientHelper(db dbh.Handle, authPath string) bool {
	body := strings.ReplaceAll(authBootstrapJS, "__AUTH_PATH__", authPath)
	a := &store.Artifact{
		Kind:        "file",
		Path:        "js/auth.js",
		Scope:       "global",
		ContentType: "application/javascript",
		Body:        body,
		Metadata:    map[string]any{"platform_managed": true},
	}
	if _, _, err := store.ArtifactSave(db, a, "platform:auth"); err != nil {
		return false
	}
	return true
}
