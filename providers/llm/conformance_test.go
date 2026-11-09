// Package llm_test holds the cross-transport conformance suite: contracts every
// registered LLM transport must satisfy, asserted against all of them at once.
//
// It is an EXTERNAL test package (llm_test, not llm) because it must import the
// backend packages to trigger their init()-time registration, and those packages
// import llm. Go permits that only from an external test package; an in-package
// test would be an import cycle.
//
// The blank imports below are also what populates the quirk-rule registry for
// the in-package drift guards, which compile into the same test binary.
package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/llm"
	_ "openfabrica/providers/llm/anthropic" // register via init()
	_ "openfabrica/providers/llm/ollama"    // register via init()
	_ "openfabrica/providers/llm/openai"    // register via init()
)

// Type() is the identity downstream consumers route on: error labels, the
// replay-cache hash, of_llm_calls, and the quirk-rule registry. Walks the
// registry so a new wrapper that forgets SetTypeName fails here.
func TestRegisteredTransportReportsItsOwnType(t *testing.T) {
	types := llm.RegisteredTypes()
	if len(types) == 0 {
		t.Fatal("no transports registered: the blank imports above are not taking effect")
	}
	for _, transport := range types {
		t.Run(transport, func(t *testing.T) {
			p, err := llm.NewProvider(transport, llm.Options{Name: "conformance", APIKey: "k", HTTPClient: &http.Client{}})
			if err != nil {
				t.Fatalf("NewProvider(%q): %v", transport, err)
			}
			if got := p.Type(); got != transport {
				t.Errorf("Type() = %q, want %q (registered key). A wrapper embedding *openai.Client must call SetTypeName in its constructor, not declare its own Type().", got, transport)
			}
			// Name() must survive the same way: it keys the capability store.
			if got := p.Name(); got != "conformance" {
				t.Errorf("Name() = %q, want the constructor's name argument", got)
			}
		})
	}
}

// Errors must name the PROVIDER ROW, not the transport. Most vendors share the
// openai-chat transport, so "openai-chat: rate limited" cannot tell an operator
// WHICH of their configured providers failed, while providers.name is UNIQUE and
// says exactly that. Drives a real 400 and checks the label.
func TestErrorsAreLabelledWithTheProviderRow(t *testing.T) {
	for _, transport := range llm.RegisteredTypes() {
		t.Run(transport, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"conformance: rejected on purpose"}}`)
			}))
			defer srv.Close()

			const rowName = "Conformance Row"
			p, err := llm.NewProvider(transport, llm.Options{Name: rowName, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("NewProvider(%q): %v", transport, err)
			}
			_, err = p.Complete(context.Background(), llm.CompletionRequest{
				Model:     "conformance-model",
				Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
				MaxTokens: 16,
			})
			if err == nil {
				t.Fatal("Complete succeeded against a server that only returns 400")
			}
			if !strings.HasPrefix(err.Error(), rowName+":") {
				t.Errorf("error label = %q, want it to start with %q.\nEvery provider client must tag llm.PostJSON with its ROW NAME, so a log line names the configured provider rather than a wire protocol most vendors share.",
					err.Error(), rowName)
			}
		})
	}
}

// quirkProbe is one transport's rejection body plus the request that provokes it,
// so the shared contracts below drive every transport's learn-and-retry path
// without knowing which field each one is about.
type quirkProbe struct {
	transport string
	rejection string
	offending func(map[string]any) bool
	req       llm.CompletionRequest
	ok        string
}

func thinkOffReq(model string) llm.CompletionRequest {
	off := false
	return llm.CompletionRequest{
		Model:     model,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		MaxTokens: 16,
		Thinking:  &off,
	}
}

func samplingReq(model string) llm.CompletionRequest {
	temp := 0.2
	return llm.CompletionRequest{
		Model:       model,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		MaxTokens:   16,
		Temperature: &temp,
	}
}

const openAIOK = `{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
const anthropicOK = `{"id":"1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func quirkProbes() []quirkProbe {
	thinkingDisabled := func(b map[string]any) bool {
		m, ok := b["thinking"].(map[string]any)
		return ok && m["type"] == "disabled"
	}
	hasTemp := func(b map[string]any) bool { _, ok := b["temperature"]; return ok }
	return []quirkProbe{
		{
			transport: "openai-chat",
			rejection: `{"error":{"code":"1210","message":"This model always engages in thinking and cannot be disabled; please use low, high, or max"}}`,
			offending: thinkingDisabled,
			req:       thinkOffReq("probe-model"),
			ok:        openAIOK,
		},
		{
			transport: "anthropic",
			rejection: `{"error":{"message":"temperature is not supported on this model"}}`,
			offending: hasTemp,
			req:       samplingReq("probe-model"),
			ok:        anthropicOK,
		},
	}
}

// probeServer answers 400 while the request still carries the offending field,
// and 200 once it stops. Counts every call.
func probeServer(t *testing.T, p quirkProbe) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if p.offending(body) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, p.rejection)
			return
		}
		io.WriteString(w, p.ok)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A rejection is learned and the retry drops the field, so the call succeeds
// rather than surfacing a fatal 400 to the engine.
func TestConformance_QuirkRejectionIsLearnedAndRetried(t *testing.T) {
	for _, p := range quirkProbes() {
		t.Run(p.transport, func(t *testing.T) {
			t.Cleanup(llm.Caps.Reset)
			srv, calls := probeServer(t, p)
			prov, err := llm.NewProvider(p.transport, llm.Options{Name: "conf-retry-" + p.transport, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			if _, err := prov.Complete(context.Background(), p.req); err != nil {
				t.Fatalf("Complete = %v, want the retry to succeed", err)
			}
			if *calls != 2 {
				t.Errorf("%d calls, want 2 (one rejected, one retried)", *calls)
			}
		})
	}
}

// The store outlives the client. engine.resolve builds a fresh client every
// session, so a per-client latch re-paid the rejected request once per stage.
// noParallelToolCalls was exactly that bug.
func TestConformance_LearnedStateOutlivesTheClient(t *testing.T) {
	for _, p := range quirkProbes() {
		t.Run(p.transport, func(t *testing.T) {
			t.Cleanup(llm.Caps.Reset)
			srv, calls := probeServer(t, p)
			name := "conf-outlives-" + p.transport

			first, _ := llm.NewProvider(p.transport, llm.Options{Name: name, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if _, err := first.Complete(context.Background(), p.req); err != nil {
				t.Fatalf("first Complete: %v", err)
			}
			before := *calls

			// A brand-new client for the same provider row and model, as the next
			// stage would build.
			second, _ := llm.NewProvider(p.transport, llm.Options{Name: name, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if _, err := second.Complete(context.Background(), p.req); err != nil {
				t.Fatalf("second Complete: %v", err)
			}
			if got := *calls - before; got != 1 {
				t.Errorf("a fresh client cost %d calls, want 1: the learned state did not outlive the client, so every stage re-pays the rejected request", got)
			}
		})
	}
}

// Two provider rows naming the same model are two deployments and must learn
// independently. Anthropic's latches keyed on the bare model name and could not
// tell them apart.
func TestConformance_LearnedStateIsScopedToTheProviderRow(t *testing.T) {
	for _, p := range quirkProbes() {
		t.Run(p.transport, func(t *testing.T) {
			t.Cleanup(llm.Caps.Reset)
			srv, calls := probeServer(t, p)

			a, _ := llm.NewProvider(p.transport, llm.Options{Name: "conf-row-a-" + p.transport, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if _, err := a.Complete(context.Background(), p.req); err != nil {
				t.Fatalf("row A Complete: %v", err)
			}
			before := *calls

			b, _ := llm.NewProvider(p.transport, llm.Options{Name: "conf-row-b-" + p.transport, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if _, err := b.Complete(context.Background(), p.req); err != nil {
				t.Fatalf("row B Complete: %v", err)
			}
			if got := *calls - before; got != 2 {
				t.Errorf("a different provider row cost %d calls, want 2: it inherited another row's learned state instead of discovering its own", got)
			}
		})
	}
}

// An unrelated 400 must not be retried at all, on any transport.
func TestConformance_UnrelatedBadRequestIsNotRetried(t *testing.T) {
	for _, transport := range llm.RegisteredTypes() {
		t.Run(transport, func(t *testing.T) {
			t.Cleanup(llm.Caps.Reset)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"context length exceeded"}}`)
			}))
			defer srv.Close()

			prov, err := llm.NewProvider(transport, llm.Options{Name: "conf-unrelated-" + transport, APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			_, err = prov.Complete(context.Background(), llm.CompletionRequest{
				Model:     "probe-model",
				Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
				MaxTokens: 16,
			})
			if err == nil {
				t.Fatal("Complete returned nil on an unrecoverable 400")
			}
			if calls != 1 {
				t.Errorf("%d calls, want 1: an unrelated 400 must not be retried", calls)
			}
		})
	}
}
