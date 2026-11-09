package api

import (
	"strings"
	"testing"
)

// TestRLSScopeWarning checks the safety-net heuristic that nudges the builder
// when an endpoint exposes a per-user table without owner/tenant scoping.
func TestRLSScopeWarning(t *testing.T) {
	// Owner-like column, no scoping, public read -> strongest warning.
	if w := rlsScopeWarning([]string{"id", "user_id", "body"}, "", "", true); w == "" {
		t.Error("expected warning for unscoped public endpoint on a user-owned table")
	} else if !strings.Contains(w, "anyone can read") {
		t.Errorf("public-read warning text unexpected: %q", w)
	}

	// Owner-like column, no scoping, auth required -> still warns (every user sees all).
	if w := rlsScopeWarning([]string{"id", "tenant_id"}, "", "", false); w == "" {
		t.Error("expected warning for unscoped authenticated endpoint on a tenant table")
	}

	// Scoped by owner_column -> no warning.
	if w := rlsScopeWarning([]string{"id", "user_id"}, "user_id", "", true); w != "" {
		t.Errorf("owner-scoped endpoint should not warn, got %q", w)
	}
	// Scoped by tenant_column -> no warning.
	if w := rlsScopeWarning([]string{"id", "tenant_id"}, "", "tenant_id", true); w != "" {
		t.Errorf("tenant-scoped endpoint should not warn, got %q", w)
	}

	// No owner-like column -> no warning (a plain content table is fine public).
	if w := rlsScopeWarning([]string{"id", "title", "body"}, "", "", true); w != "" {
		t.Errorf("table with no owner-like column should not warn, got %q", w)
	}

	// Case-insensitive match.
	if w := rlsScopeWarning([]string{"ID", "Owner_ID"}, "", "", false); w == "" {
		t.Error("expected case-insensitive match on Owner_ID")
	}
}
