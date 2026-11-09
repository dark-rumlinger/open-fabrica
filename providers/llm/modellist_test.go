package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/internal/providercfg"
)

// The Gemini listing pages at 50 by default. The parser must ask for a large
// page and follow nextPageToken, or models past the first page never appear.
func TestListGemini_FollowsPages(t *testing.T) {
	var sawPageSize, sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sawPageSize = q.Get("pageSize")
		switch q.Get("pageToken") {
		case "":
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-a","displayName":"A","inputTokenLimit":1000000,"outputTokenLimit":65536,"supportedGenerationMethods":["generateContent"],"thinking":true}],"nextPageToken":"p2"}`)
		case "p2":
			sawToken = "p2"
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-b","supportedGenerationMethods":["generateContent"]},{"name":"models/embed","supportedGenerationMethods":["embedContent"]}]}`)
		default:
			t.Errorf("unexpected page token %q", q.Get("pageToken"))
		}
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), ModelListSpec{
		HTTPClient: srv.Client(), URL: srv.URL + "/v1beta/models", Shape: providercfg.ShapeGemini, Label: "gemini",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawPageSize != "1000" || sawToken != "p2" {
		t.Errorf("pageSize=%q token=%q, want 1000 and p2", sawPageSize, sawToken)
	}
	if len(got) != 2 || got[0].ID != "gemini-a" || got[1].ID != "gemini-b" {
		t.Fatalf("got %+v, want gemini-a and gemini-b (embed-only model filtered)", got)
	}
	if !got[0].SupportsReasoning || got[0].ContextTokens != 1000000 {
		t.Errorf("first model = %+v, want reasoning and a 1M window", got[0])
	}
}

// OpenRouter reports prices per token as strings; they must land as USD per
// million, with the router placeholder "-1" read as unknown.
func TestListOpenRouter_PricingAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"a/m1","name":"M1","context_length":200000,"top_provider":{"max_completion_tokens":64000},
			 "pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003"},
			 "architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools","reasoning"]},
			{"id":"openrouter/auto","pricing":{"prompt":"-1","completion":"-1"}}]}`)
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), ModelListSpec{
		HTTPClient: srv.Client(), URL: srv.URL, Shape: providercfg.ShapeOpenRouter, Label: "openrouter",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got[0]
	if m.Cost.Input != 3 || m.Cost.Output != 15 || m.Cost.CacheRead != 0.3 {
		t.Errorf("cost = %+v, want 3 / 15 / 0.3 per million", m.Cost)
	}
	if m.MaxOutputTokens != 64000 || m.ContextTokens != 200000 || !m.SupportsVision || !m.SupportsReasoning || !m.SupportsTools {
		t.Errorf("model = %+v", m)
	}
	if got[1].Cost != (ModelCost{}) {
		t.Errorf("router placeholder priced at %+v, want zero", got[1].Cost)
	}
}

func TestPerMillion(t *testing.T) {
	for in, want := range map[string]float64{"0.000003": 3, "": 0, "abc": 0, "-1": 0, "0": 0} {
		if got := perMillion(in); got != want {
			t.Errorf("perMillion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWithQuery_KeepsExistingParams(t *testing.T) {
	got := WithQuery("http://h/p?key=abc", "pageSize", "1000")
	if got != "http://h/p?key=abc&pageSize=1000" {
		t.Errorf("got %s", got)
	}
}
