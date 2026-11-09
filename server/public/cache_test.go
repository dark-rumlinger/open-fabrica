package public

import (
	"strings"
	"testing"
)

// TestFormatCacheKeyScopesByCaller guards the response-cache isolation fix: the
// key MUST fold in the caller's owner/tenant scope so a response computed under
// one caller's row-level filter is never replayed to a different caller (a
// cross-user / cross-tenant disclosure). Unscoped (nil,nil) responses share one
// identity-free key.
func TestFormatCacheKeyScopesByCaller(t *testing.T) {
	path, query := "/api/notes", "limit=50"

	// Different owners -> different keys.
	kA := formatCacheKey(1, "notes", 5, nil, path, query)
	kB := formatCacheKey(1, "notes", 6, nil, path, query)
	if kA == kB {
		t.Fatalf("owner 5 and owner 6 share a cache key (%q) - one caller's rows would leak to the other", kA)
	}

	// Different tenants -> different keys.
	tA := formatCacheKey(1, "notes", nil, 10, path, query)
	tB := formatCacheKey(1, "notes", nil, 11, path, query)
	if tA == tB {
		t.Fatalf("tenant 10 and tenant 11 share a cache key (%q) - cross-tenant leak", tA)
	}

	// Anonymous / unscoped requests share one identity-free key.
	anon1 := formatCacheKey(1, "notes", nil, nil, path, query)
	anon2 := formatCacheKey(1, "notes", nil, nil, path, query)
	if anon1 != anon2 {
		t.Fatalf("unscoped requests must share a key: %q vs %q", anon1, anon2)
	}
	if anon1 == kA {
		t.Fatalf("an unscoped key collides with an owner-scoped key (%q) - the owner's rows would serve to anonymous callers", anon1)
	}

	// The table-level invalidation prefix must remain a prefix of every per-caller
	// variant so a write still evicts them all.
	prefix := formatInvalidationPrefix(1, "notes")
	for _, k := range []string{kA, kB, tA, tB, anon1} {
		if !strings.HasPrefix(k, prefix) {
			t.Fatalf("cache key %q lost the invalidation prefix %q - invalidateTable would miss it", k, prefix)
		}
	}
}
