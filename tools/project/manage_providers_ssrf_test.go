package project

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// providerTestDB is a migrated project database, so of_service_providers carries
// exactly the columns provider.Load reads in production.
func providerTestDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

func addProvider(t *testing.T, db dbh.Handle, name, baseURL string, allowPrivate int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO of_service_providers (name, base_url, auth_type, is_enabled, allow_private_host)
		 VALUES (?, ?, 'none', 1, ?)`,
		name, baseURL, allowPrivate,
	); err != nil {
		t.Fatalf("insert provider %s: %v", name, err)
	}
}

// TestProviderRequestSSRFGuard is the SSRF-guard end-to-end check. A loopback target is
// blocked by default (proving manage_providers.request is now SSRF-guarded,
// which it was not before), and the same target is reachable only when the
// provider was registered with allow_private_host. The host is scoped: an
// allowed provider can reach its own host but the block still applies otherwise.
func TestProviderRequestSSRFGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	// srv.URL is http://127.0.0.1:PORT - a loopback (private) host.

	db := providerTestDB(t)
	addProvider(t, db, "blocked", srv.URL, 0)
	addProvider(t, db, "lan", srv.URL, 1)
	ctx := &tools.ToolContext{DB: db}

	// Default provider: loopback target is rejected before any dial.
	res, err := (&ProviderRequest{Provider: "blocked", Method: "GET", Path: "/x"}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "blocked URL") {
		t.Fatalf("expected loopback to be blocked by default, got %+v", res)
	}

	// Opted-in provider: the same host is reachable and the call round-trips.
	res, err = (&ProviderRequest{Provider: "lan", Method: "GET", Path: "/x"}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected allow_private_host provider to reach its host, got %+v", res)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data, got %T", res.Data)
	}
	if body, _ := data["body"].(string); !strings.Contains(body, "ok") {
		t.Fatalf("expected response body to round-trip, got %v", data["body"])
	}
}
