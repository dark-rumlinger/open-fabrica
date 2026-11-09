package llm

import (
	"path/filepath"
	"testing"

	"openfabrica/db"
)

// newCapsDB opens a fresh migrated database in a temp dir. Uses the real
// migrations so the providers / llm_models schema matches production exactly
// and the capability query is exercised against the same DDL it runs on.
func newCapsDB(t *testing.T) *db.DB {
	t.Helper()
	dbh, err := db.Open(filepath.Join(t.TempDir(), "caps.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return dbh
}

// addProvider inserts an enabled, keyless provider of the given kind and
// returns its id. Keyless keeps the availability predicate satisfied without
// having to encrypt a key, so the tests isolate the model-readiness clause.
func addProvider(t *testing.T, dbh *db.DB, name, kind string) int64 {
	t.Helper()
	res, err := dbh.ExecWrite(
		`INSERT INTO providers (name, vendor, transport, kind, is_enabled, requires_api_key)
		 VALUES (?, ?, ?, ?, 1, 0)`,
		name, name, kind, kind,
	)
	if err != nil {
		t.Fatalf("insert provider %s: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// addModel attaches a model row to a provider, enabled or not.
func addModel(t *testing.T, dbh *db.DB, providerID int64, enabled bool) {
	t.Helper()
	en := 0
	if enabled {
		en = 1
	}
	if _, err := dbh.ExecWrite(
		`INSERT INTO llm_models (provider_id, model_id, display_name, is_enabled) VALUES (?, 'm1', 'M1', ?)`,
		providerID, en,
	); err != nil {
		t.Fatalf("insert model: %v", err)
	}
}

// TestLoadCapabilities_ImageRequiresEnabledModel locks in the tightened image
// gate: an image provider only flips HasImage when it also owns an enabled
// model, matching what manage_image_gen needs to resolve a request. Without
// this, the seeded keyless Automatic1111 provider surfaced a tool that could
// only fail at call time.
func TestLoadCapabilities_ImageRequiresEnabledModel(t *testing.T) {
	t.Run("no model => image stays off", func(t *testing.T) {
		dbh := newCapsDB(t)
		addProvider(t, dbh, "A1111", "image") // keyless, no model (seeded shape)
		caps, err := LoadCapabilities(dbh.DB)
		if err != nil {
			t.Fatalf("LoadCapabilities: %v", err)
		}
		if caps.HasImage {
			t.Fatal("HasImage = true; want false when the image provider has no enabled model")
		}
	})

	t.Run("enabled model => image on", func(t *testing.T) {
		dbh := newCapsDB(t)
		id := addProvider(t, dbh, "A1111", "image")
		addModel(t, dbh, id, true)
		caps, err := LoadCapabilities(dbh.DB)
		if err != nil {
			t.Fatalf("LoadCapabilities: %v", err)
		}
		if !caps.HasImage {
			t.Fatal("HasImage = false; want true when the image provider has an enabled model")
		}
	})

	t.Run("only a disabled model => image stays off", func(t *testing.T) {
		dbh := newCapsDB(t)
		id := addProvider(t, dbh, "A1111", "image")
		addModel(t, dbh, id, false)
		caps, err := LoadCapabilities(dbh.DB)
		if err != nil {
			t.Fatalf("LoadCapabilities: %v", err)
		}
		if caps.HasImage {
			t.Fatal("HasImage = true; want false when the image provider's only model is disabled")
		}
	})
}

// TestLoadCapabilities_NonImageKindsArePresenceGated documents that the model
// clause is deliberately image-only. Stock/search seed rows ship with a
// default model, and llm gating is model-independent, so a future change must
// not extend the readiness clause to these kinds.
func TestLoadCapabilities_NonImageKindsArePresenceGated(t *testing.T) {
	dbh := newCapsDB(t)
	addProvider(t, dbh, "Ollama", "llm")    // keyless llm, no model row
	addProvider(t, dbh, "Pixabay", "stock") // keyless stock, no model row
	addProvider(t, dbh, "Tavily", "search") // keyless search, no model row
	caps, err := LoadCapabilities(dbh.DB)
	if err != nil {
		t.Fatalf("LoadCapabilities: %v", err)
	}
	if !caps.HasLLM {
		t.Error("HasLLM = false; want true (llm is presence-gated, not model-gated)")
	}
	if !caps.HasStock {
		t.Error("HasStock = false; want true (stock is presence-gated, not model-gated)")
	}
	if !caps.HasSearch {
		t.Error("HasSearch = false; want true (search is presence-gated, not model-gated)")
	}
	if caps.HasImage {
		t.Error("HasImage = true; want false (no image provider configured)")
	}
}
