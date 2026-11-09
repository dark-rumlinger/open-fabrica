package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/providercfg"
)

const seedPath = "../../seed.json"

// The shipped seed.json must load cleanly: every row validates, vendors are
// unique, and plans are known. A typo here otherwise fails at first boot.
func TestShippedSeedLoads(t *testing.T) {
	raw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg SeedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	// Env-provided keys would need an encryptor; none is wanted here.
	for _, p := range cfg.Providers {
		if p.Auth.Env != "" {
			t.Setenv(p.Auth.Env, "")
		}
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := LoadSeed(seedPath, nil, database.DB, nil, nil, nil); err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}

	rows, err := models.ListProviders(database.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(cfg.Providers) {
		t.Fatalf("%d rows for %d seed entries: duplicate vendor or name?", len(rows), len(cfg.Providers))
	}
	for _, r := range rows {
		if !models.ValidPlan(r.Plan) {
			t.Errorf("%s: plan %q", r.Vendor, r.Plan)
		}
		if _, err := providercfg.ParseStrict(r.Config); err != nil {
			t.Errorf("%s: %v", r.Vendor, err)
		}
	}
}

// The curated LLM set: exactly these vendors, and only the coding rows carry
// the coding plan.
func TestShippedSeedLLMVendors(t *testing.T) {
	raw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg SeedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"anthropic": "standard", "openai": "standard", "ollama": "standard", "llamacpp": "standard",
		"gemini": "standard", "openrouter": "standard", "zai": "standard",
		"zai-coding": "coding", "kimi-coding": "coding", "minimax-coding": "coding", "alibaba-coding": "coding",
	}
	got := map[string]string{}
	for _, p := range cfg.Providers {
		if p.Kind != "llm" {
			continue
		}
		plan := p.Plan
		if plan == "" {
			plan = "standard"
		}
		got[p.Vendor] = plan
		if p.Config.EmbeddingModel != "" && p.Vendor != "openai" {
			t.Errorf("%s declares an embedding model; only openai should", p.Vendor)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("llm vendors = %v, want %v", got, want)
	}
	for v, plan := range want {
		if got[v] != plan {
			t.Errorf("%s: plan %q, want %q", v, got[v], plan)
		}
	}
}

// With no seed.json on disk (a bare binary, a container) the embedded copy is
// applied, so the instance still starts with its providers.
func TestLoadSeedFallsBackToEmbedded(t *testing.T) {
	raw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg SeedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Providers {
		if p.Auth.Env != "" {
			t.Setenv(p.Auth.Env, "")
		}
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	missing := filepath.Join(t.TempDir(), "absent.json")
	if err := LoadSeed(missing, raw, database.DB, nil, nil, nil); err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	rows, err := models.ListProviders(database.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(cfg.Providers) {
		t.Fatalf("embedded fallback seeded %d rows, want %d", len(rows), len(cfg.Providers))
	}
}
