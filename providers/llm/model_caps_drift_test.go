package llm

// Drift guards for the capability layer. Each closes a gap that let something
// rot silently before:
//   - a supports_* column written, shown in the admin UI, and never read;
//   - a capability id nothing can ever set;
//   - the UNIQUE constraint the whole key design rests on being dropped.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", rel)
}

// tableBody returns the CREATE TABLE body for name, so columns of the same name
// on other tables are not matched.
func tableBody(t *testing.T, schema, name string) string {
	t.Helper()
	i := strings.Index(schema, "CREATE TABLE IF NOT EXISTS "+name)
	if i < 0 {
		t.Fatalf("table %s not found in schema", name)
	}
	rest := schema[i:]
	j := strings.Index(rest, ");")
	if j < 0 {
		t.Fatalf("could not find end of %s table definition", name)
	}
	return rest[:j]
}

// supports_vision was written by discovery, returned by the admin API, rendered
// as a badge, and never once consulted at request time. Nothing failed, because
// nothing was checking. This asserts every supports_* column on llm_models has a
// reader in the seeding file, so the next one added cannot go dead the same way.
func TestSupportsColumnsHaveRuntimeReaders(t *testing.T) {
	schema, err := os.ReadFile(repoFile(t, "db/migrations/001_initial.sql"))
	if err != nil {
		t.Fatalf("reading schema: %v", err)
	}
	seed, err := os.ReadFile(repoFile(t, "providers/llm/model_caps_seed.go"))
	if err != nil {
		t.Fatalf("reading seed file: %v", err)
	}

	body := tableBody(t, string(schema), "llm_models")
	cols := regexp.MustCompile(`(?m)^\s*(supports_\w+)`).FindAllStringSubmatch(body, -1)
	if len(cols) == 0 {
		t.Fatal("no supports_* columns found on llm_models: has the schema moved?")
	}
	for _, m := range cols {
		field := goFieldForColumn(m[1])
		if !strings.Contains(string(seed), field) {
			t.Errorf("llm_models.%s has no reader in model_caps_seed.go (expected %s).\nA supports_* column that is written and never read is dead weight the admin UI still shows as meaningful.", m[1], field)
		}
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
// registered quirk rule or from DB seeding.
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

	for cap, name := range capConstNames() {
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
func capConstNames() map[string]string {
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
		CapForcedToolChoice:  "CapForcedToolChoice",
		CapNativeThinkField:  "CapNativeThinkField",
	}
	// Guard against the two lists drifting apart.
	for cap := range KnownModelCaps {
		if _, ok := all[cap]; !ok {
			all[cap] = ""
		}
	}
	return all
}

// ModelKey uses providers.name as the provider half, which is only sound while
// that column is UNIQUE: without it, two rows could share a name and silently
// merge their learned state. Nothing else in the codebase would notice.
func TestProviderNameIsUnique(t *testing.T) {
	schema, err := os.ReadFile(repoFile(t, "db/migrations/001_initial.sql"))
	if err != nil {
		t.Fatalf("reading schema: %v", err)
	}
	body := tableBody(t, string(schema), "providers")
	if !regexp.MustCompile(`(?i)name\s+TEXT\s+NOT NULL\s+UNIQUE`).MatchString(body) {
		t.Error("providers.name is no longer NOT NULL UNIQUE.\nllm.ModelKey keys the capability store on it; without uniqueness two provider rows sharing a name would merge their learned capabilities.")
	}
}
