package engine

import (
	"reflect"
	"testing"

	"openfabrica/providers/llm"
)

func sampleRequest() llm.CompletionRequest {
	temp := 0.1
	return llm.CompletionRequest{
		Model:       "test-model",
		System:      "You are a builder.",
		MaxTokens:   4096,
		Temperature: &temp,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "build a page"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "t1", Name: "manage_files", Arguments: `{"action":"save"}`}}},
			{Role: llm.RoleTool, ToolCallID: "t1", Content: `{"ok":true}`},
		},
		Tools: []llm.ToolDef{
			{
				Name:        "manage_files",
				Description: "Save files",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"zeta":   map[string]any{"type": "string"},
						"alpha":  map[string]any{"type": "number"},
						"action": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}

// TestRequestHashDeterminism pins that hashing the same request twice -
// including map-typed tool parameters, whose key order Go randomizes at
// iteration time - yields identical digests (encoding/json sorts map keys).
func TestRequestHashDeterminism(t *testing.T) {
	a := RequestHash("anthropic", sampleRequest())
	for i := 0; i < 32; i++ {
		b := RequestHash("anthropic", sampleRequest())
		if a != b {
			t.Fatalf("hash unstable across marshals: %s vs %s", a, b)
		}
	}
	if a == "" {
		t.Fatal("hash empty")
	}
}

// TestRequestHashSensitivity: any semantic change must change the hash.
func TestRequestHashSensitivity(t *testing.T) {
	base := RequestHash("anthropic", sampleRequest())

	cases := map[string]func(*llm.CompletionRequest){
		"model":       func(r *llm.CompletionRequest) { r.Model = "other-model" },
		"system":      func(r *llm.CompletionRequest) { r.System += "!" },
		"message":     func(r *llm.CompletionRequest) { r.Messages[0].Content += "!" },
		"tool order":  func(r *llm.CompletionRequest) { r.Tools = append(r.Tools, llm.ToolDef{Name: "extra"}) },
		"max tokens":  func(r *llm.CompletionRequest) { r.MaxTokens++ },
		"temperature": func(r *llm.CompletionRequest) { v := 0.9; r.Temperature = &v },
		"tool params": func(r *llm.CompletionRequest) { r.Tools[0].Parameters["new"] = true },
		"top p":       func(r *llm.CompletionRequest) { v := 0.5; r.TopP = &v },
		"stop words":  func(r *llm.CompletionRequest) { r.StopWords = []string{"STOP"} },
		"thinking":    func(r *llm.CompletionRequest) { v := false; r.Thinking = &v },
		// Effort reaches the wire on providers that expose it, so two efforts are
		// two requests.
		"effort": func(r *llm.CompletionRequest) { r.Effort = "high" },
		"schema added": func(r *llm.CompletionRequest) {
			r.ResponseSchema = &llm.ResponseSchema{Name: "p", Schema: []byte(`{}`)}
		},
	}
	for name, mutate := range cases {
		req := sampleRequest()
		mutate(&req)
		if got := RequestHash("anthropic", req); got == base {
			t.Errorf("%s change did not change hash", name)
		}
	}

	if RequestHash("openai", sampleRequest()) == base {
		t.Error("provider type change did not change hash")
	}
}

// TestRequestHashIgnoresRoutingHints: cache-control placement and routing
// hints must NOT change the hash (they don't change the response).
func TestRequestHashIgnoresRoutingHints(t *testing.T) {
	base := RequestHash("anthropic", sampleRequest())

	req := sampleRequest()
	req.PromptCacheKey = "warm-shard-7"
	req.CacheSystem = true
	req.StablePrefixBytes = 9999
	req.Messages[0].Cacheable = true
	if got := RequestHash("anthropic", req); got != base {
		t.Error("routing hints changed the hash")
	}
}

// hashExcludedFields are the CompletionRequest fields RequestHash deliberately
// ignores, each with the reason it cannot change the provider's response. Adding
// a field here is a decision that must be argued; the default is to hash it.
var hashExcludedFields = map[string]string{
	"CacheSystem":       "cache-control placement; does not change the response",
	"PromptCacheKey":    "routing hint for prefix-cache shard affinity; not semantics",
	"StablePrefixBytes": "prefix-stability diagnostic; providers never read it",
	"StablePrefixMsgs":  "prefix-stability diagnostic; providers never read it",
	"TotalRequestBytes": "prefix-stability diagnostic; providers never read it",
}

// TestEveryCompletionRequestFieldIsClassified reflects over CompletionRequest
// and fails on any field neither proven hash-affecting nor listed as a
// deliberate exclusion, so a field added to the request cannot reach the
// provider unhashed with nothing to catch it. A field counts as hash-affecting
// only if mutating it actually changes the digest, so naming one here without
// wiring it into RequestHash does not satisfy this test.
func TestEveryCompletionRequestFieldIsClassified(t *testing.T) {
	// mutators produce a request differing from sampleRequest() in exactly the
	// named field. Every non-excluded field of CompletionRequest needs one.
	mutators := map[string]func(*llm.CompletionRequest){
		"Model":       func(r *llm.CompletionRequest) { r.Model = "other-model" },
		"Messages":    func(r *llm.CompletionRequest) { r.Messages[0].Content += "!" },
		"System":      func(r *llm.CompletionRequest) { r.System += "!" },
		"Tools":       func(r *llm.CompletionRequest) { r.Tools = append(r.Tools, llm.ToolDef{Name: "extra"}) },
		"MaxTokens":   func(r *llm.CompletionRequest) { r.MaxTokens++ },
		"Temperature": func(r *llm.CompletionRequest) { v := 0.9; r.Temperature = &v },
		"TopP":        func(r *llm.CompletionRequest) { v := 0.5; r.TopP = &v },
		"StopWords":   func(r *llm.CompletionRequest) { r.StopWords = []string{"STOP"} },
		"Thinking":    func(r *llm.CompletionRequest) { v := false; r.Thinking = &v },
		"Effort":      func(r *llm.CompletionRequest) { r.Effort = "high" },
		"ResponseSchema": func(r *llm.CompletionRequest) {
			r.ResponseSchema = &llm.ResponseSchema{Name: "p", Schema: []byte(`{}`)}
		},
	}

	base := RequestHash("anthropic", sampleRequest())
	rt := reflect.TypeOf(llm.CompletionRequest{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if reason, excluded := hashExcludedFields[name]; excluded {
			if _, alsoMutated := mutators[name]; alsoMutated {
				t.Errorf("field %s is both excluded (%s) and asserted hash-affecting; pick one", name, reason)
			}
			continue
		}
		mutate, ok := mutators[name]
		if !ok {
			t.Errorf("CompletionRequest.%s is unclassified: either hash it in RequestHash and add a mutator "+
				"here, or add it to hashExcludedFields with the reason it cannot change the response", name)
			continue
		}
		req := sampleRequest()
		mutate(&req)
		if RequestHash("anthropic", req) == base {
			t.Errorf("CompletionRequest.%s is declared hash-affecting but mutating it did not change the "+
				"digest - it is missing from hashedRequest/RequestHash", name)
		}
	}

	// The excluded set must not name fields that no longer exist, or it silently
	// grants an exemption to nothing while the real field goes unchecked.
	for name := range hashExcludedFields {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("hashExcludedFields names %s, which is not a CompletionRequest field", name)
		}
	}
}
