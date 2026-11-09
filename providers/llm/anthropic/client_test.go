package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/llm"
)

// A configured cache_ttl rides every breakpoint object; absent config keeps the
// plain 5m ephemeral form.
func TestBuildRequestBody_CacheTTL(t *testing.T) {
	req := llm.CompletionRequest{
		Model:       "claude-x",
		System:      "system prompt",
		CacheSystem: true,
		Tools: []llm.ToolDef{
			{Name: "manage_files", Description: "d", Parameters: map[string]any{"type": "object"}},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "intent", Cacheable: true},
			{Role: llm.RoleUser, Content: "task"},
		},
	}

	withTTL := &Client{cfg: providercfg.Config{CacheTTL: "1h"}}
	b, err := json.Marshal(withTTL.buildRequestBody(req, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"ttl":"1h"`) {
		t.Error("configured cache_ttl should appear on the breakpoint objects")
	}

	plain := &Client{}
	b, err = json.Marshal(plain.buildRequestBody(req, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"ttl"`) {
		t.Error("without cache_ttl config no ttl field should be emitted")
	}
}

// TestParseSSEStream_PreservesInputAndCacheTokens guards the streaming usage
// accounting. Anthropic reports input + cache counts once in message_start and
// the final cumulative output_tokens in message_delta; a regression that
// replaced (rather than merged) usage on message_delta zeroed the input and
// dropped the cache split, which under-metered the public LLM proxy's daily
// budget for streaming Anthropic endpoints.
func TestParseSSEStream_PreservesInputAndCacheTokens(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"output_tokens":2,"cache_read_input_tokens":5000,"cache_creation_input_tokens":300}}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":150}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var final *llm.Usage
	c := &Client{}
	err := c.parseSSEStream(strings.NewReader(stream), func(chunk llm.StreamChunk) {
		if chunk.Done && chunk.Usage != nil {
			final = chunk.Usage
		}
	})
	if err != nil {
		t.Fatalf("parseSSEStream returned error: %v", err)
	}
	if final == nil {
		t.Fatal("no usage captured from stream")
	}
	if final.InputTokens != 1200 {
		t.Errorf("InputTokens = %d, want 1200 (message_delta must not zero it)", final.InputTokens)
	}
	if final.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150 (final cumulative from message_delta)", final.OutputTokens)
	}
	if final.CacheReadInputTokens != 5000 {
		t.Errorf("CacheReadInputTokens = %d, want 5000", final.CacheReadInputTokens)
	}
	if final.CacheCreationInputTokens != 300 {
		t.Errorf("CacheCreationInputTokens = %d, want 300", final.CacheCreationInputTokens)
	}
}

// countCacheControl tallies every cache_control: ephemeral marker buildRequestBody
// emits across system, tools, and message content blocks.
func countCacheControl(body map[string]any) int {
	n := 0
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if _, ok := t["cache_control"]; ok {
				n++
			}
			for _, vv := range t {
				walk(vv)
			}
		case map[string]string:
			// leaf maps carry no nested cache_control
		case []map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(body)
	return n
}

// Anthropic rejects a request with more than 4 cache_control breakpoints. The
// build loop's shape - cached system + last tool + several Cacheable user messages
// (intent, stable pre-inline, snapshot) - would naively emit 5, so buildRequestBody
// must cap the user-message breakpoints and keep only the tail-most ones.
func TestBuildRequestBody_CapsCacheBreakpointsAtFour(t *testing.T) {
	c := &Client{}
	req := llm.CompletionRequest{
		Model:       "claude-x",
		System:      "system prompt",
		CacheSystem: true,
		Tools: []llm.ToolDef{
			{Name: "find_tool", Description: "d", Parameters: map[string]any{"type": "object"}},
			{Name: "manage_files", Description: "d", Parameters: map[string]any{"type": "object"}},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "intent", Cacheable: true},
			{Role: llm.RoleUser, Content: "stable pre-inline", Cacheable: true},
			{Role: llm.RoleUser, Content: "task instruction"},
			{Role: llm.RoleUser, Content: "owned pre-inline"},
			{Role: llm.RoleAssistant, Content: "ok"},
			{Role: llm.RoleUser, Content: "project_state snapshot", Cacheable: true},
		},
	}

	body := c.buildRequestBody(req, false)
	if got := countCacheControl(body); got != 4 {
		t.Fatalf("want exactly 4 cache_control breakpoints (cap), got %d", got)
	}

	// The kept user breakpoints must be the tail-most Cacheable messages (stable +
	// snapshot), since a later breakpoint's prefix subsumes the earlier intent.
	msgs, _ := body["messages"].([]map[string]any)
	hasCC := func(m map[string]any) bool {
		blocks, ok := m["content"].([]map[string]any)
		if !ok || len(blocks) == 0 {
			return false
		}
		_, ok = blocks[len(blocks)-1]["cache_control"]
		return ok
	}
	// First Cacheable user message ("intent") must have been dropped.
	if hasCC(msgs[0]) {
		t.Error("the earliest Cacheable user message (intent) should NOT keep a breakpoint under the cap")
	}
	// The last Cacheable user message ("snapshot", index 5) must keep its breakpoint.
	if !hasCC(msgs[5]) {
		t.Error("the tail-most Cacheable user message (snapshot) must keep its breakpoint")
	}
}

// With CacheSystem set, the final message of the request always carries a
// breakpoint regardless of role or Cacheable flag: with an append-only history
// it covers the whole conversation, so the next call reads everything up to
// here from cache even on turns that append no Cacheable message.
func TestBuildRequestBody_StampsFinalMessage(t *testing.T) {
	c := &Client{}
	req := llm.CompletionRequest{
		Model:       "claude-x",
		System:      "system prompt",
		CacheSystem: true,
		Tools: []llm.ToolDef{
			{Name: "manage_files", Description: "d", Parameters: map[string]any{"type": "object"}},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "intent", Cacheable: true},
			{Role: llm.RoleAssistant, Content: "saving", ToolCalls: []llm.ToolCall{{ID: "t1", Name: "manage_files", Arguments: `{"action":"save"}`}}},
			{Role: llm.RoleTool, Content: `{"success":true}`, ToolCallID: "t1"},
		},
	}

	body := c.buildRequestBody(req, false)
	msgs, _ := body["messages"].([]map[string]any)
	last := msgs[len(msgs)-1]
	blocks, ok := last["content"].([]map[string]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("final tool-result message must have content blocks, got %#v", last["content"])
	}
	if _, ok := blocks[len(blocks)-1]["cache_control"]; !ok {
		t.Error("the final message of the request must carry a cache breakpoint")
	}
	// system + last tool + final message + the one Cacheable user message = 4.
	if got := countCacheControl(body); got != 4 {
		t.Fatalf("want 4 breakpoints, got %d", got)
	}
}

// The cap holds when the final message is itself a Cacheable user message: the
// tail reservation covers it and the freed user budget goes to the next
// tail-most Cacheable message, never exceeding 4 total.
func TestBuildRequestBody_FinalCacheableMessageNotDoubleCounted(t *testing.T) {
	c := &Client{}
	req := llm.CompletionRequest{
		Model:       "claude-x",
		System:      "system prompt",
		CacheSystem: true,
		Tools: []llm.ToolDef{
			{Name: "manage_files", Description: "d", Parameters: map[string]any{"type": "object"}},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "intent", Cacheable: true},
			{Role: llm.RoleUser, Content: "shell", Cacheable: true},
			{Role: llm.RoleUser, Content: "snapshot", Cacheable: true},
		},
	}

	body := c.buildRequestBody(req, false)
	if got := countCacheControl(body); got != 4 {
		t.Fatalf("want exactly 4 breakpoints, got %d", got)
	}
	msgs, _ := body["messages"].([]map[string]any)
	hasCC := func(m map[string]any) bool {
		blocks, ok := m["content"].([]map[string]any)
		if !ok || len(blocks) == 0 {
			return false
		}
		_, ok = blocks[len(blocks)-1]["cache_control"]
		return ok
	}
	if !hasCC(msgs[2]) {
		t.Error("the final message must carry the tail breakpoint")
	}
	if !hasCC(msgs[1]) {
		t.Error("the freed user budget must go to the next tail-most Cacheable message")
	}
	if hasCC(msgs[0]) {
		t.Error("the earliest Cacheable message must be dropped under the cap")
	}
}

// Without CacheSystem the system/tools blocks take no breakpoints, so all the
// Cacheable user messages (up to 4) keep theirs.
func TestBuildRequestBody_NoSystemCacheKeepsUserBreakpoints(t *testing.T) {
	c := &Client{}
	req := llm.CompletionRequest{
		Model: "claude-x",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "a", Cacheable: true},
			{Role: llm.RoleUser, Content: "b", Cacheable: true},
			{Role: llm.RoleUser, Content: "c", Cacheable: true},
		},
	}
	body := c.buildRequestBody(req, false)
	if got := countCacheControl(body); got != 3 {
		t.Fatalf("want 3 cache_control breakpoints (all user messages, no base markers), got %d", got)
	}
}

// Pure learn-from-400: there is no model-name list. Sampling params and effort
// are sent by DEFAULT to every model; only a model LEARNED (via a live 400) to
// reject one has it dropped, and the two are independent. Uses distinct model
// names per case so the process-global capability store does not cross-pollute.
func TestBuildRequestBody_SamplingAndEffortSentUntilLearned(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := New(llm.Options{Name: "test", APIKey: "key"})
	temp := 0.2
	mk := func(model string) map[string]any {
		return c.buildRequestBody(llm.CompletionRequest{
			Model:       model,
			Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
			Temperature: &temp,
			Effort:      "medium",
		}, false)
	}
	hasEffort := func(b map[string]any) bool { _, ok := b["output_config"]; return ok }
	hasTemp := func(b map[string]any) bool { _, ok := b["temperature"]; return ok }

	// Default (nothing learned): both sent - no model names decide this.
	b := mk("test-model-default")
	if !hasTemp(b) || !hasEffort(b) {
		t.Errorf("by default both temperature and effort are sent, got temp=%v effort=%v", hasTemp(b), hasEffort(b))
	}

	// Learning sampling-removed drops temperature, leaves effort untouched.
	llm.Caps.Learn(c.capKey("test-model-nosampling"), llm.CapSamplingParams, llm.No)
	b = mk("test-model-nosampling")
	if hasTemp(b) {
		t.Error("temperature must be dropped once the model is learned to reject it")
	}
	if !hasEffort(b) {
		t.Error("effort is independent - it must still be sent")
	}

	// Learning effort-unsupported drops effort, leaves temperature untouched.
	llm.Caps.Learn(c.capKey("test-model-noeffort"), llm.CapEffort, llm.No)
	b = mk("test-model-noeffort")
	if hasEffort(b) {
		t.Error("effort must be dropped once the model is learned to reject it")
	}
	if !hasTemp(b) {
		t.Error("temperature is independent - it must still be sent")
	}

	// Empty Effort never emits output_config, learned or not.
	b = c.buildRequestBody(llm.CompletionRequest{
		Model: "test-model-noeffort-arg", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, false)
	if hasEffort(b) {
		t.Error("empty Effort must not emit output_config")
	}
}

// A model that rejects sampling params is LEARNED from its 400 and the request
// retried once without them; later requests to that model omit the params from
// the first attempt. (No model-name list exists - every model is treated this
// way.)
func TestComplete_LearnsSamplingRemovalAndRetries(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"temperature"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"` + "`temperature`" + ` is deprecated for this model."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	c := New(llm.Options{Name: "test", APIKey: "key", BaseURL: server.URL, HTTPClient: server.Client()})
	temp := 0.0
	req := llm.CompletionRequest{
		Model:       "claude-imaginary-6-learned-test", // any model: sent, then learned-and-dropped
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Temperature: &temp,
		MaxTokens:   16,
	}

	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete must learn the rejection and succeed on retry, got: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 requests (rejected + retried), got %d", len(bodies))
	}
	if strings.Contains(bodies[1], `"temperature"`) {
		t.Fatal("retry must omit temperature")
	}

	// The removal is memoized: the next call omits the params on attempt one.
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("memoized call failed: %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("memoized call must be a single request, got %d total", len(bodies))
	}
	if strings.Contains(bodies[2], `"temperature"`) {
		t.Fatal("memoized call must omit temperature from the first attempt")
	}
}

// An unrelated 400 must NOT trigger the sampling retry - it keeps its normal
// fatal classification and surfaces after exactly one attempt.
func TestComplete_UnrelatedBadRequestNotRetried(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`))
	}))
	defer server.Close()

	c := New(llm.Options{Name: "test", APIKey: "key", BaseURL: server.URL, HTTPClient: server.Client()})
	temp := 0.5
	_, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:       "claude-imaginary-6-unrelated-test",
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Temperature: &temp,
		MaxTokens:   16,
	})
	if err == nil {
		t.Fatal("want the 400 surfaced as an error")
	}
	if calls != 1 {
		t.Fatalf("unrelated 400 must not be retried, got %d calls", calls)
	}
}

// Anthropic's forced-tool input_schema rejects oneOf/allOf/anyOf at the top
// level (400: "input_schema does not support oneOf, allOf, or anyOf at the top
// level"). The engine's PLAN schema carries a top-level anyOf; the client must
// strip it from the emitted input_schema while leaving nested combinators (and
// every other property) intact.
func TestBuildRequestBody_StripsTopLevelCombinatorsFromToolSchema(t *testing.T) {
	c := New(llm.Options{Name: "test", APIKey: "key"})
	schema := `{
		"type": "object",
		"anyOf": [{"required": ["tasks"]}, {"required": ["questions"]}],
		"properties": {
			"tasks": {"type": "array"},
			"mode":  {"anyOf": [{"const": "a"}, {"const": "b"}]}
		}
	}`
	body := c.buildRequestBody(llm.CompletionRequest{
		Model:          "claude-opus-4-8",
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		ResponseSchema: &llm.ResponseSchema{Name: "plan_response", Schema: []byte(schema)},
	}, false)

	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("want one forced tool, got %#v", body["tools"])
	}
	inSchema, ok := tools[0]["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema is not an object: %#v", tools[0]["input_schema"])
	}
	if _, present := inSchema["anyOf"]; present {
		t.Error("top-level anyOf must be stripped (the API rejects it)")
	}
	if _, present := inSchema["properties"]; !present {
		t.Error("properties must survive the strip")
	}
	// A nested anyOf (inside a property) is legal and must be preserved.
	props := inSchema["properties"].(map[string]any)
	mode := props["mode"].(map[string]any)
	if _, present := mode["anyOf"]; !present {
		t.Error("nested anyOf inside a property must be preserved")
	}
}

// A model that rejects effort is LEARNED from its 400, dropped, and the request
// retried; later requests omit effort from the first attempt. Effort is sent
// optimistically to every model, so no special name is needed to trigger it.
func TestComplete_LearnsEffortRemovalAndRetries(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"output_config"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"output_config.effort: effort is not supported for this model"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	c := New(llm.Options{Name: "test", APIKey: "key", BaseURL: server.URL, HTTPClient: server.Client()})
	// Any model gets effort sent optimistically; the server rejects it here.
	req := llm.CompletionRequest{
		Model:     "claude-effort-reject-test",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Effort:    "medium",
		MaxTokens: 16,
	}

	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete must learn the effort rejection and succeed on retry, got: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 requests (rejected + retried), got %d", len(bodies))
	}
	if strings.Contains(bodies[1], `"output_config"`) {
		t.Fatal("retry must omit output_config")
	}
	// Memoized: next call omits effort on attempt one.
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("memoized call failed: %v", err)
	}
	if len(bodies) != 3 || strings.Contains(bodies[2], `"output_config"`) {
		t.Fatalf("memoized call must be a single request without output_config, got %d bodies", len(bodies))
	}
}

// With forced tool_choice learned off (Sonnet 5.5, Opus 5.5, Fable 5.1), the single
// response tool is kept but chosen via tool_choice=auto, and native
// output_config.format is NOT used (its strict schema subset would reject the
// engine's lenient response schemas).
func TestBuildRequestBody_AutoToolChoiceWhenForcedOff(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := New(llm.Options{Name: "test", APIKey: "key"})
	llm.Caps.Learn(llm.ModelKey{Provider: "test", Model: "claude-sonnet-5-5"}, llm.CapForcedToolChoice, llm.No)

	schema := `{"type":"object","properties":{"goal":{"type":"string"}}}`
	body := c.buildRequestBody(llm.CompletionRequest{
		Model:          "claude-sonnet-5-5",
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		ResponseSchema: &llm.ResponseSchema{Name: "plan_response", Schema: []byte(schema)},
	}, false)

	if _, present := body["output_config"]; present {
		t.Error("native output_config.format must not be used (its strict schema subset would reject lenient schemas)")
	}
	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("the single response tool must still be sent, got %#v", body["tools"])
	}
	tc, ok := body["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "auto" {
		t.Fatalf("tool_choice must be auto, got %#v", body["tool_choice"])
	}
}

// The newer models reject a forced tool_choice: the 400 is learned once and the
// structured-output request retried with tool_choice=auto (the tool is unchanged).
func TestComplete_LearnsForcedToolChoiceRejectionAndRetries(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), `"type":"tool"`) { // a forced tool_choice
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"tool_choice: type \"tool\" and \"any\" are not supported for this model."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"plan_response","input":{"goal":"ok"}}],"model":"m","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	c := New(llm.Options{Name: "test", APIKey: "key", BaseURL: server.URL, HTTPClient: server.Client()})
	req := llm.CompletionRequest{
		Model:          "claude-sonnet-5-5",
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		ResponseSchema: &llm.ResponseSchema{Name: "plan_response", Schema: []byte(`{"type":"object"}`)},
		MaxTokens:      16,
	}
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete must learn the forced-tool rejection and succeed on retry, got: %v", err)
	}
	if resp.Content != `{"goal":"ok"}` {
		t.Fatalf("content = %q, want the tool input JSON", resp.Content)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 requests (rejected + retried), got %d", len(bodies))
	}
	if strings.Contains(bodies[1], `"type":"tool"`) {
		t.Error("retry must not force the tool (expected tool_choice auto)")
	}
	if !strings.Contains(bodies[1], `"tool_choice"`) {
		t.Error("retry should still send tool_choice (auto)")
	}
}

// ListModels reads per-model max_tokens and vision from the Models API
// capabilities tree, and falls back gracefully when a model object omits them
// (an older API version or a compatible proxy) - so no model name is baked in
// where the API answers, and behavior degrades safely where it doesn't.
func TestListModels_ReadsCapabilitiesWithFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"claude-new-a","display_name":"New A","max_tokens":128000,"capabilities":{"image_input":{"supported":true}}},
			{"id":"claude-text-b","display_name":"Text B","max_tokens":40000,"capabilities":{"image_input":{"supported":false}}},
			{"id":"claude-bare-c","display_name":"Bare C"}
		]}`))
	}))
	defer server.Close()

	c := New(llm.Options{Name: "test", APIKey: "key", BaseURL: server.URL, HTTPClient: server.Client()})
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]llm.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	// The API's max_tokens is the OUTPUT cap, so it must land in
	// MaxOutputTokens - never in ContextTokens, which is the window. Reading
	// one as the other is the exact confusion the split column removed.
	//
	// A: reported max_tokens + vision flag = true, both read from the API.
	if a := byID["claude-new-a"]; a.MaxOutputTokens != 128000 || !a.SupportsVision {
		t.Errorf("A: got maxOutput=%d vision=%v, want 128000/true", a.MaxOutputTokens, a.SupportsVision)
	}
	// B: a text-only model's image_input.supported=false is honored (not overridden).
	if b := byID["claude-text-b"]; b.MaxOutputTokens != 40000 || b.SupportsVision {
		t.Errorf("B: got maxOutput=%d vision=%v, want 40000/false", b.MaxOutputTokens, b.SupportsVision)
	}
	// C: no max_tokens, no capabilities -> the model-agnostic fallback cap and
	// vision defaults true (every current Claude model is multimodal).
	if cc := byID["claude-bare-c"]; cc.MaxOutputTokens != fallbackMaxOutputTokens || !cc.SupportsVision {
		t.Errorf("C: got maxOutput=%d vision=%v, want %d/true (fallback)", cc.MaxOutputTokens, cc.SupportsVision, fallbackMaxOutputTokens)
	}
	// None of these fixtures reports max_input_tokens, so the WINDOW stays
	// unknown rather than silently inheriting the output cap.
	for id, m := range byID {
		if m.ContextTokens != 0 {
			t.Errorf("%s: ContextTokens=%d, want 0 - the fixture reports no max_input_tokens, and the output cap must not leak into the window", id, m.ContextTokens)
		}
	}
	// Streaming and tool use have no capabilities flag - always true.
	for _, m := range models {
		if !m.SupportsStreaming || !m.SupportsTools {
			t.Errorf("%s: streaming/tools must stay true", m.ID)
		}
	}
}

// The Models API pages at 20 by default. ListModels must ask for the maximum
// page size and follow has_more/last_id, or everything past the first page is
// silently missing from the picker.
func TestListModels_FollowsPagination(t *testing.T) {
	var limits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits = append(limits, r.URL.Query().Get("limit"))
		switch r.URL.Query().Get("after_id") {
		case "":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a","max_input_tokens":1000000,"capabilities":{"thinking":{"supported":true}}}],"has_more":true,"last_id":"claude-a"}`))
		case "claude-a":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-b"}],"has_more":false,"last_id":"claude-b"}`))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("after_id"))
		}
	}))
	defer server.Close()

	c := New(llm.Options{Name: "test", APIKey: "key", BaseURL: server.URL, HTTPClient: server.Client()})
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "claude-a" || models[1].ID != "claude-b" {
		t.Fatalf("models = %+v, want both pages", models)
	}
	for _, l := range limits {
		if l != "1000" {
			t.Errorf("limit = %q, want 1000", l)
		}
	}
	if models[0].ContextTokens != 1000000 || !models[0].SupportsReasoning {
		t.Errorf("first model = %+v, want a 1M window and reasoning", models[0])
	}
}

// An assistant turn with neither text nor tool calls must not reach the wire:
// Anthropic rejects empty message content.
func TestBuildRequestBody_DropsEmptyAssistantTurn(t *testing.T) {
	c := &Client{}
	body := c.buildRequestBody(llm.CompletionRequest{
		Model: "claude-x",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "start"},
			{Role: llm.RoleAssistant, Content: ""},
			{Role: llm.RoleUser, Content: "continue"},
		},
	}, false)
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (empty assistant turn dropped)", len(msgs))
	}
	for _, m := range msgs {
		if m["role"] == "assistant" {
			t.Errorf("an empty assistant message reached the wire: %v", m)
		}
	}
}
