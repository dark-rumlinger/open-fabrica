package admin

import (
	"net/http"
	"strings"
	"testing"

	_ "openfabrica/providers/llm/openai"
	_ "openfabrica/providers/websearch/brave"
)

func TestNonLLMTransportsIncludeWebSearch(t *testing.T) {
	if !isNonLLMTransport("brave") {
		t.Error("a web-search transport must be accepted when adding a provider")
	}
	if isNonLLMTransport("openai-chat") {
		t.Error("an LLM transport is not a non-LLM one")
	}
}

func TestAddSearchProviderAndTestIt(t *testing.T) {
	deps := newBotTestDeps(t)
	h := &ProvidersHandler{deps: deps}
	rec := doJSON(h.Create, "POST", "/providers", `{"name":"My Brave","transport":"brave","api_key":"k"}`, nil)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("adding a search provider: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(h.TestConfig, "POST", "/providers/test", `{"transport":"brave","api_key":"k"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("testing a search provider config should accept it without a model: %d %s", rec.Code, rec.Body.String())
	}
}
