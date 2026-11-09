package engine

// Golden prompt bytes: provider prompt caches key on a byte-stable prefix, so a
// refactor that moves one byte of a system prompt, tool schema, or seed silently
// re-bills every build at full price. This drives the offline EXPAND -> PLAN ->
// BUILD scenario and pins every request the fake provider received against
// testdata/golden. A diff here is a cost regression until explained.
//
// Regenerate after an intended prompt change:
//
//	go test ./engine -run TestGoldenPromptBytes -update

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

var updateGolden = flag.Bool("update", false, "rewrite engine/testdata/golden instead of comparing")

// goldenScrubbers blank the per-run values that legitimately differ between two
// identical builds (ids minted from the clock, temp dirs). Everything else must
// be reproducible byte for byte.
var goldenScrubbers = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`run_\d+_\d+`), "run_<id>"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`), "<timestamp>"},
	// The owner seed carries "Today (UTC): <date>"; without this the snapshot fails
	// every new day. After the timestamp rule so a full timestamp stays one token.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), "<date>"},
}

func scrubGolden(s string, tempDir string) string {
	if tempDir != "" {
		s = strings.ReplaceAll(s, tempDir, "<tmp>")
		s = strings.ReplaceAll(s, filepath.ToSlash(tempDir), "<tmp>")
	}
	for _, sc := range goldenScrubbers {
		s = sc.re.ReplaceAllString(s, sc.repl)
	}
	return s
}

// renderGoldenRequest lays one request out as readable text: the fields that
// reach the provider's cache key, in wire order.
func renderGoldenRequest(i int, req llm.CompletionRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== request %d ===\n", i)
	fmt.Fprintf(&b, "model: %s\nmax_tokens: %d\ncache_system: %v\nprompt_cache_key: %s\n",
		req.Model, req.MaxTokens, req.CacheSystem, req.PromptCacheKey)
	if req.Temperature != nil {
		fmt.Fprintf(&b, "temperature: %v\n", *req.Temperature)
	}
	if req.Thinking != nil {
		fmt.Fprintf(&b, "thinking: %v\n", *req.Thinking)
	}
	if req.ResponseSchema != nil {
		sch, _ := json.Marshal(req.ResponseSchema)
		fmt.Fprintf(&b, "response_schema: %s\n", sch)
	}
	fmt.Fprintf(&b, "--- system (%d bytes) ---\n%s\n", len(req.System), req.System)
	fmt.Fprintf(&b, "--- tools (%d) ---\n", len(req.Tools))
	for _, td := range req.Tools {
		params, _ := json.Marshal(td.Parameters)
		fmt.Fprintf(&b, "%s: %s\n  params: %s\n", td.Name, td.Description, params)
	}
	fmt.Fprintf(&b, "--- messages (%d) ---\n", len(req.Messages))
	for j, m := range req.Messages {
		fmt.Fprintf(&b, "[%d] %s cacheable=%v", j, m.Role, m.Cacheable)
		if m.ToolCallID != "" {
			fmt.Fprintf(&b, " tool_call_id=%s", m.ToolCallID)
		}
		b.WriteString("\n")
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "  call %s %s %s\n", tc.ID, tc.Name, tc.Arguments)
		}
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func TestGoldenPromptBytes(t *testing.T) {
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	defer gdb.Close()
	mgr := db.NewProjectDBManager(dir)
	defer mgr.CloseAll()
	// user_flows on: flow tests are an opt-in owner setting, so without this the build
	// would skip the user-flows checklist item and reject write_flows (user_flows_off),
	// and the golden would pin that rejection instead of the real flow path and the
	// flows-conditional build seed that a live interactive build carries.
	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level, config)
		 VALUES (1, 'Tip Calc', 'a one-page tip calculator', 1, 'building', 1, 'high', '{"user_flows":true}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	provider := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("A polished one-page tip calculator with bill input, percentage presets, and split-by-people."), // EXPAND
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSON), // PLAN
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"built the calculator page"}`}),
		enginetest.Text("The page probe is skipped in this offline run."), // page-probe nudge ack, build stops
		// Post-build flow gate: write of_test.js for the finished app, then a plain-text
		// turn ends it. This pins the gate session's prompt (its when:flows build frame).
		userFlowsStep,
		enginetest.Text("Flows saved."),
	}}

	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	worker, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	validatorMu.Lock()
	saved := validatorSet
	validatorSet = nil
	validatorMu.Unlock()
	defer func() {
		validatorMu.Lock()
		validatorSet = saved
		validatorMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)
	if got := worker.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}
	// One LIVE owner-chat wake on the finished project pins the LIVE prompt and
	// preload set too. The script is spent, so the fake answers with plain text.
	worker.handleChatWake(ctx, "make the heading blue")
	worker.dbLog.Close()

	goldenDir := filepath.Join("testdata", "golden")
	if *updateGolden {
		if err := os.RemoveAll(goldenDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i, req := range provider.Requests {
		got := scrubGolden(renderGoldenRequest(i, req), dir)
		path := filepath.Join(goldenDir, fmt.Sprintf("build_request_%02d.txt", i))
		if *updateGolden {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing golden %s (run with -update): %v", path, err)
		}
		if w := strings.ReplaceAll(string(want), "\r\n", "\n"); w != got {
			t.Errorf("request %d drifted from %s: %s", i, path, firstDiff(w, got))
		}
	}
	if !*updateGolden {
		entries, _ := os.ReadDir(goldenDir)
		if len(entries) != len(provider.Requests) {
			t.Errorf("golden has %d requests, run made %d (run with -update if intended)", len(entries), len(provider.Requests))
		}
	}
}

// firstDiff reports the first differing line with a little context so a drift
// failure names the moved bytes instead of dumping two whole prompts.
func firstDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("line %d\n  want: %q\n  got:  %q", i+1, w, g)
		}
	}
	return "identical lines, differing line endings"
}
