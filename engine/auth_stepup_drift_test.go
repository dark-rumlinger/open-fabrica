package engine

// Drift guard for the auth contract's login step-up shapes.
//
// The auth handler answers a login that needs one more step with HTTP 200,
// `success: false`, a `needs_*` marker, and NO token; a marker missing from the
// `auth-window-api` contract produces a generated login handler that stores
// `data.token` on a 200 and breaks the session silently. The contract's own
// probe cannot catch this (a probe only asserts a substring is PRESENT), so
// parse the handler for the markers it actually emits and assert each is
// written down somewhere the model reads. Either home counts: the always-on
// contract carries the LAW (gate the token store on `success`, not on the
// status); the per-feature recovery mechanics live in the auth_contract guide.
// Asserting the union keeps this guard from forcing conditional 2FA/tenancy
// detail into every project's prompt.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/pack"
)

// stepUpKeyRe captures the `"needs_x": true` JSON keys the auth handler writes
// into a tokenless 200 response.
var stepUpKeyRe = regexp.MustCompile(`"(needs_[a-z_]+)"`)

func TestAuthContractCoversLoginStepUpShapes(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate server/public/auth.go")
	}
	root := filepath.Dir(thisFile)

	var found []string
	seen := map[string]bool{}
	for _, rel := range []string{"auth.go", "auth_tenancy.go", "auth_totp.go", "auth_magic.go"} {
		path := filepath.Join(root, "..", "server", "public", rel)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, m := range stepUpKeyRe.FindAllStringSubmatch(string(src), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				found = append(found, m[1])
			}
		}
	}
	sort.Strings(found)

	// Guard the guard: if the parse finds nothing, the assertions below are vacuous.
	if len(found) < 3 {
		t.Fatalf("parsed only %d needs_* markers from the auth handler (%v) - did the response shape change?", len(found), found)
	}

	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	c, ok := all["auth-window-api"]
	if !ok {
		t.Fatal("auth-window-api contract not found")
	}

	packs, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	var guide string
	for _, p := range packs {
		if p.Slug == "auth_contract" {
			guide = p.Body
			break
		}
	}
	if guide == "" {
		t.Fatal("auth_contract guide not found")
	}

	for _, key := range found {
		if !strings.Contains(c.Body, key) && !strings.Contains(guide, key) {
			t.Errorf("neither the auth-window-api contract nor the auth_contract guide documents "+
				"the %q login step-up shape.\n"+
				"The handler returns it as a 200 with success:false and no token, so a generated "+
				"login handler stores an undefined token and the session breaks silently. Document "+
				"the marker and how the client recovers from it.", key)
		}
	}
}
