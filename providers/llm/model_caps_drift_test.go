package llm

// Drift guards for the capability layer. Each closes a gap that let something
// rot silently before:
//   - a supports_* column written, shown in the admin UI, and never read;
//   - a capability id nothing can ever set;
//   - the UNIQUE constraint the whole key design rests on being dropped.
//
// The schema-side checks read the real migrated schema (PRAGMA), not the SQL text,
// so a reformat cannot break them and a real drift cannot hide behind one.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"openfabrica/db"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", rel)
}

// migratedDB opens a throwaway global database with the real schema applied.
func migratedDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "caps.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// supports_vision was written by discovery, returned by the admin API, rendered
// as a badge, and never once consulted at request time. Nothing failed, because
// nothing was checking. This asserts every supports_* column on llm_models has a
// reader in the seeding file, so the next one added cannot go dead the same way.
func TestSupportsColumnsHaveRuntimeReaders(t *testing.T) {
	seed, err := os.ReadFile(repoFile(t, "providers/llm/model_caps_seed.go"))
	if err != nil {
		t.Fatalf("reading seed file: %v", err)
	}

	database := migratedDB(t)
	rows, err := database.DB.Query(`PRAGMA table_info(llm_models)`)
	if err != nil {
		t.Fatalf("table_info(llm_models): %v", err)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if !strings.HasPrefix(name, "supports_") {
			continue
		}
		found++
		field := goFieldForColumn(name)
		if !strings.Contains(string(seed), field) {
			t.Errorf("llm_models.%s has no reader in model_caps_seed.go (expected %s).\nA supports_* column that is written and never read is dead weight the admin UI still shows as meaningful.", name, field)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if found == 0 {
		t.Fatal("no supports_* columns found on llm_models: has the schema moved?")
	}
}

// goFieldForColumn maps supports_foo_bar to SupportsFooBar.
func goFieldForColumn(col string) string {
	var b strings.Builder
	for _, part := range strings.Split(col, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// A capability nobody can ever set is dead weight that reads as meaningful at
// every call site checking it. Every id must be reachable either from a
// registered quirk rule or from DB seeding. "Declared in seeding" is a source
// fact (the seed is Go that builds rules), so this one stays a source check.
func TestEveryKnownModelCapHasASource(t *testing.T) {
	seed, err := os.ReadFile(repoFile(t, "providers/llm/model_caps_seed.go"))
	if err != nil {
		t.Fatalf("reading seed file: %v", err)
	}

	fromRules := map[string]bool{}
	quirkMu.RLock()
	for _, rules := range quirkRules {
		for _, r := range rules {
			fromRules[r.Cap] = true
		}
	}
	quirkMu.RUnlock()

	// CapStructuredOutput is demoted directly by the openai schema ladder rather
	// than by a registered rule, so it is named here explicitly.
	fromCode := map[string]bool{CapStructuredOutput: true}

	for cap, name := range capConstNames(t) {
		if fromRules[cap] || fromCode[cap] {
			continue
		}
		if strings.Contains(string(seed), name) {
			continue
		}
		t.Errorf("capability %q has no source: no quirk rule sets it and DB seeding does not declare it", cap)
	}
}

// capConstNames maps each known capability id to the Go constant naming it, so
// the source check looks for the constant rather than a bare string literal.
func capConstNames(t *testing.T) map[string]string {
	t.Helper()
	all := map[string]string{
		CapTools:             "CapTools",
		CapVision:            "CapVision",
		CapStreaming:         "CapStreaming",
		CapStructuredOutput:  "CapStructuredOutput",
		CapParallelToolCalls: "CapParallelToolCalls",
		CapSamplingParams:    "CapSamplingParams",
		CapMaxTokensField:    "CapMaxTokensField",
		CapThinkingOff:       "CapThinkingOff",
		CapThinkingField:     "CapThinkingField",
		CapReasoningEffort:   "CapReasoningEffort",
		CapEffort:            "CapEffort",
		CapReasoning:         "CapReasoning",
		CapForcedToolChoice:  "CapForcedToolChoice",
		CapNativeThinkField:  "CapNativeThinkField",
	}
	// The two lists must agree. A capability missing here would get an empty
	// name, and an empty name matches any source text.
	for cap := range KnownModelCaps {
		if _, ok := all[cap]; !ok {
			t.Errorf("capability %q is in KnownModelCaps but not in capConstNames", cap)
		}
	}
	for cap := range all {
		if !KnownModelCaps[cap] {
			t.Errorf("capability %q is in capConstNames but not in KnownModelCaps", cap)
		}
	}
	return all
}

// ModelKey uses providers.name as the provider half, which is only sound while
// that column is UNIQUE: without it, two rows could share a name and silently
// merge their learned state. This inserts two rows with the same name (and
// different vendor/transport, so only the name can collide) into the real
// migrated schema and requires the second to be rejected.
func TestProviderNameIsUnique(t *testing.T) {
	database := migratedDB(t)
	if _, err := database.DB.Exec(
		`INSERT INTO providers (name, vendor, transport) VALUES ('dup', 'v1', 't1')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := database.DB.Exec(
		`INSERT INTO providers (name, vendor, transport) VALUES ('dup', 'v2', 't2')`); err == nil {
		t.Error("providers.name accepted a duplicate: it is no longer UNIQUE.\nllm.ModelKey keys the capability store on it; two provider rows sharing a name would merge their learned capabilities.")
	}
}
