package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"openfabrica/db/models"
	"openfabrica/providers/llm"
)

// capsFake answers the reasoning probe with a reasoning token and the tools probe
// with a tool call, or fails every call as rate limited; it counts the calls.
type capsFake struct {
	calls     atomic.Int32
	throttled bool
}

func (p *capsFake) Name() string                       { return "Z" }
func (p *capsFake) Type() string                       { return "openai-chat" }
func (p *capsFake) Ping(context.Context, string) error { return nil }
func (p *capsFake) Complete(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.calls.Add(1)
	if p.throttled {
		return nil, &llm.Error{Code: llm.CodeRateLimit, Status: 429}
	}
	return &llm.CompletionResponse{ToolCalls: []llm.ToolCall{{Name: "add_numbers", Arguments: `{"a":17,"b":25}`}}}, nil
}
func (p *capsFake) Stream(_ context.Context, _ llm.CompletionRequest, cb llm.StreamCallback) error {
	p.calls.Add(1)
	if p.throttled {
		return &llm.Error{Code: llm.CodeRateLimit, Status: 429}
	}
	cb(llm.StreamChunk{Reasoning: "thinking"})
	return nil
}

// newProbeProvider makes an id-only (plain OpenAI listing) provider with the given
// enabled and disabled models, its live client registered as fake.
func newProbeProvider(t *testing.T, fake llm.Provider, enabled, disabled []string) (*Deps, *models.LLMProvider) {
	t.Helper()
	deps := newBotTestDeps(t)
	deps.LLMRegistry = llm.NewRegistry()
	p, err := models.CreateProvider(deps.DB.DB, "Z", "zai-coding", "openai-chat", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	deps.LLMRegistry.Register("Z", fake)
	for _, set := range []struct {
		ids []string
		on  bool
	}{{enabled, true}, {disabled, false}} {
		for _, id := range set.ids {
			if _, err := deps.DB.DB.Exec(`INSERT INTO llm_models (provider_id, model_id, display_name, context_tokens, is_enabled) VALUES (?, ?, ?, 8000, ?)`, p.ID, id, id, set.on); err != nil {
				t.Fatal(err)
			}
		}
	}
	return deps, p
}

func modelByID(t *testing.T, deps *Deps, providerID int, id string) *models.LLMModel {
	t.Helper()
	rows, err := models.ListModelsByProvider(deps.DB.DB, providerID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].ModelID == id {
			return &rows[i]
		}
	}
	t.Fatalf("no model %s", id)
	return nil
}

// A sync never probes, and keeps what earlier probes settled even though the
// listing it writes says nothing about reasoning.
func TestSyncKeepsVerdictsWithoutProbing(t *testing.T) {
	fake := &capsFake{}
	deps, p := newProbeProvider(t, fake, []string{"glm-5"}, nil)
	m := modelByID(t, deps, p.ID, "glm-5")
	if err := models.SetProbeVerdict(deps.DB.DB, m.ID, m.Config, "reasoning", true); err != nil {
		t.Fatal(err)
	}
	if _, err := syncLLMCatalog(deps, p.ID, []llm.ModelInfo{{ID: "glm-5", SupportsTools: true}, {ID: "glm-6"}}, false); err != nil {
		t.Fatal(err)
	}
	if n := fake.calls.Load(); n != 0 {
		t.Fatalf("a sync made %d probe calls, want none", n)
	}
	if !modelByID(t, deps, p.ID, "glm-5").SupportsReasoning {
		t.Fatal("the stored reasoning verdict must survive the sync")
	}
}

// Listings that report capabilities, and capabilities the admin set by hand, are
// never probed; a stored verdict is probed again only on request.
func TestProbeCapsFor(t *testing.T) {
	deps, p := newProbeProvider(t, &capsFake{}, []string{"m"}, nil)
	m := modelByID(t, deps, p.ID, "m")
	if got := probeCapsFor(p, m, false); len(got) != 2 {
		t.Fatalf("an unprobed model on an id-only listing needs both probes, got %v", got)
	}
	for _, reported := range []*models.LLMProvider{
		{Kind: "llm", Transport: "anthropic"},
		{Kind: "llm", Transport: "openai-chat", Config: `{"models_shape":"openrouter"}`},
	} {
		if got := probeCapsFor(reported, m, true); got != nil {
			t.Errorf("%s/%s reports capabilities itself, got probes %v", reported.Transport, reported.Config, got)
		}
	}
	yes := true
	if err := models.MergeModelOverrides(deps.DB.DB, m.ID, m.Config, models.ModelOverrides{SupportsTools: &yes}); err != nil {
		t.Fatal(err)
	}
	m = modelByID(t, deps, p.ID, "m")
	if err := models.SetProbeVerdict(deps.DB.DB, m.ID, m.Config, "reasoning", false); err != nil {
		t.Fatal(err)
	}
	m = modelByID(t, deps, p.ID, "m")
	if got := probeCapsFor(p, m, false); len(got) != 0 {
		t.Fatalf("a set-by-hand and a probed capability need nothing, got %v", got)
	}
	if got := probeCapsFor(p, m, true); len(got) != 1 || got[0] != "reasoning" {
		t.Fatalf("a forced check re-probes the probed capability only, got %v", got)
	}
}

// The dialog's probe settles both capabilities once; opening it again costs nothing.
func TestProbeModelEndpoint(t *testing.T) {
	fake := &capsFake{}
	deps, p := newProbeProvider(t, fake, []string{"glm-5"}, nil)
	h := &ProvidersHandler{deps: deps}
	m := modelByID(t, deps, p.ID, "glm-5")
	params := map[string]string{"providerID": strconv.Itoa(p.ID), "modelID": strconv.Itoa(m.ID)}

	rec := doJSON(h.ProbeModel, "POST", "/probe", "", params)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe: %d %s", rec.Code, rec.Body.String())
	}
	var got modelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.SupportsReasoning || !got.SupportsTools || len(got.ProbePending) != 0 {
		t.Fatalf("after the probe: reasoning=%v tools=%v pending=%v", got.SupportsReasoning, got.SupportsTools, got.ProbePending)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("probe calls = %d, want 2", n)
	}
	doJSON(h.ProbeModel, "POST", "/probe", "", params)
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("a settled model was probed again: %d calls", n)
	}
}

// A provider-wide check covers enabled models only and stops once the provider
// rate limits, leaving the rest for later.
func TestProbeProviderModelsStopsOnRateLimit(t *testing.T) {
	fake := &capsFake{throttled: true}
	enabled := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	deps, p := newProbeProvider(t, fake, enabled, []string{"off"})
	h := &ProvidersHandler{deps: deps}
	rec := doJSON(h.ProbeProviderModels, "POST", "/probe-models", "", map[string]string{"providerID": strconv.Itoa(p.ID)})
	if rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Checked     int  `json:"checked"`
		Total       int  `json:"total"`
		RateLimited bool `json:"rate_limited"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != len(enabled) || !out.RateLimited || out.Checked >= out.Total {
		t.Fatalf("got %+v: want all %d enabled models queued, a rate-limit stop before the end", out, len(enabled))
	}
	if _, probed := modelByID(t, deps, p.ID, "off").ProbeVerdict("tools"); probed {
		t.Fatal("a disabled model must not be probed")
	}
}
