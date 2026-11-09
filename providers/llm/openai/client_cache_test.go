package openai

import (
	"testing"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/llm"
)

// countCacheControl tallies every cache_control marker buildRequestBody emits
// across the system and message content blocks.
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

func sampleReq() llm.CompletionRequest {
	return llm.CompletionRequest{
		Model:       "some-model",
		System:      "system prompt",
		CacheSystem: true,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "intent", Cacheable: true},
			{Role: llm.RoleUser, Content: "stable pre-inline", Cacheable: true},
			{Role: llm.RoleUser, Content: "task instruction"},
			{Role: llm.RoleAssistant, Content: "ok"},
			{Role: llm.RoleUser, Content: "project_state snapshot", Cacheable: true},
		},
	}
}

// Default openai-chat client (automatic-cache backends): no cache_control is
// emitted and content stays as plain strings, byte-identical to the pre-change
// shape so the provider's automatic prefix matcher is undisturbed.
func TestBuildRequestBody_NoCacheControlByDefault(t *testing.T) {
	c := &Client{}
	body := c.buildRequestBody(sampleReq(), false)

	if got := countCacheControl(body); got != 0 {
		t.Fatalf("default client must emit no cache_control, got %d", got)
	}
	msgs, _ := body["messages"].([]map[string]any)
	for i, m := range msgs {
		if _, ok := m["content"].(string); !ok {
			t.Errorf("message %d content should stay a plain string when caching is off, got %T", i, m["content"])
		}
	}
}

// With explicit caching on, the system block plus the tail-most Cacheable user
// messages carry breakpoints, and the total is capped at the 4-breakpoint limit.
func TestBuildRequestBody_EmitsAndCapsCacheControl(t *testing.T) {
	c := &Client{emitCacheControl: true}
	// Force the cap: system (1) + 4 Cacheable user messages would be 5.
	req := sampleReq()
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleUser, Content: "extra stable", Cacheable: true},
		llm.Message{Role: llm.RoleUser, Content: "another snapshot", Cacheable: true},
	)

	body := c.buildRequestBody(req, false)
	if got := countCacheControl(body); got != 4 {
		t.Fatalf("want exactly 4 cache_control breakpoints (cap), got %d", got)
	}

	// System breakpoint present.
	sys := body["messages"].([]map[string]any)[0]
	if sys["role"] != "system" {
		t.Fatalf("expected first message to be system, got %v", sys["role"])
	}
	if _, ok := sys["content"].([]map[string]any); !ok {
		t.Error("cached system prompt must be promoted to a content-part array")
	}

	// The last Cacheable user message must keep its breakpoint; the first must
	// have been dropped under the cap (a later breakpoint subsumes it).
	msgs := body["messages"].([]map[string]any)
	hasCC := func(m map[string]any) bool {
		blocks, ok := m["content"].([]map[string]any)
		if !ok || len(blocks) == 0 {
			return false
		}
		_, ok = blocks[len(blocks)-1]["cache_control"]
		return ok
	}
	if !hasCC(msgs[len(msgs)-1]) {
		t.Error("the tail-most Cacheable user message must keep its breakpoint")
	}
	if hasCC(msgs[1]) { // first Cacheable user message ("intent")
		t.Error("the earliest Cacheable user message should be dropped under the cap")
	}
}

// With CacheSystem on, the FINAL message carries the reserved tail breakpoint
// regardless of role or Cacheable flag, mirroring the Anthropic client: with an
// append-only history it covers the whole conversation for the next call.
func TestBuildRequestBody_StampsFinalMessage(t *testing.T) {
	c := &Client{emitCacheControl: true}
	req := llm.CompletionRequest{
		Model:       "some-model",
		System:      "system prompt",
		CacheSystem: true,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "intent", Cacheable: true},
			{Role: llm.RoleAssistant, Content: "saving", ToolCalls: []llm.ToolCall{{ID: "t1", Name: "manage_files", Arguments: `{"action":"save"}`}}},
			{Role: llm.RoleTool, Content: `{"success":true}`, ToolCallID: "t1"},
		},
	}
	body := c.buildRequestBody(req, false)
	msgs := body["messages"].([]map[string]any)
	last := msgs[len(msgs)-1]
	blocks, ok := last["content"].([]map[string]any)
	if !ok || len(blocks) == 0 {
		t.Fatal("final message content should be promoted to parts for the tail breakpoint")
	}
	if _, ok := blocks[len(blocks)-1]["cache_control"]; !ok {
		t.Error("final message must carry the reserved tail breakpoint")
	}
	if got := countCacheControl(body); got > 4 {
		t.Fatalf("breakpoints exceed the 4 cap: %d", got)
	}

	// Off switches: the default client stamps nothing; CacheSystem=false keeps
	// Cacheable user stamps but drops the reserved final-message stamp (its
	// final message here is a tool result, not Cacheable).
	if got := countCacheControl((&Client{}).buildRequestBody(req, false)); got != 0 {
		t.Fatalf("default client must not stamp, got %d", got)
	}
	req.CacheSystem = false
	noSys := c.buildRequestBody(req, false)
	lastOff := noSys["messages"].([]map[string]any)
	if blocks, ok := lastOff[len(lastOff)-1]["content"].([]map[string]any); ok && len(blocks) > 0 {
		if _, hasCC := blocks[len(blocks)-1]["cache_control"]; hasCC {
			t.Error("CacheSystem=false must not stamp the final message")
		}
	}
}

// A gateway with cache_ttl set (OpenRouter -> Anthropic) stamps the extended-cache
// lifetime on every breakpoint it emits, so the warmed prefix survives the multi-minute
// gaps between the build, repair and flow-gate sessions. Without cache_ttl the plain
// form is emitted, byte-identical to before, so a gateway that never set it is unaffected.
func TestBuildRequestBody_CacheTTL(t *testing.T) {
	ttl := &Client{emitCacheControl: true, cfg: providercfg.Config{CacheTTL: "1h"}}
	body := ttl.buildRequestBody(sampleReq(), false)
	n, withTTL := 0, 0
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if cc, ok := t["cache_control"].(map[string]string); ok {
				n++
				if cc["ttl"] == "1h" {
					withTTL++
				}
			}
			for _, vv := range t {
				walk(vv)
			}
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
	if n == 0 || withTTL != n {
		t.Fatalf("every one of the %d breakpoints must carry ttl=1h, got %d with it", n, withTTL)
	}

	// Default (no cache_ttl): breakpoints carry no ttl field.
	plain := &Client{emitCacheControl: true}
	for _, v := range plain.buildRequestBody(sampleReq(), false)["messages"].([]map[string]any) {
		if blocks, ok := v["content"].([]map[string]any); ok {
			for _, b := range blocks {
				if cc, ok := b["cache_control"].(map[string]string); ok {
					if _, has := cc["ttl"]; has {
						t.Error("a client with no cache_ttl must not stamp a ttl")
					}
				}
			}
		}
	}
}

// Non-Cacheable user messages and assistant turns never get a breakpoint even
// with caching on.
func TestBuildRequestBody_OnlyCacheableUserMessagesStamped(t *testing.T) {
	c := &Client{emitCacheControl: true}
	req := llm.CompletionRequest{
		Model: "some-model",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "plain"},
			{Role: llm.RoleAssistant, Content: "reply"},
		},
	}
	body := c.buildRequestBody(req, false)
	if got := countCacheControl(body); got != 0 {
		t.Fatalf("no Cacheable messages, want 0 breakpoints, got %d", got)
	}
}
