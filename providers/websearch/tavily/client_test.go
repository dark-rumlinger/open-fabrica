package tavily

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/websearch"
)

// A search must POST /search with the key in the JSON body, the query,
// max_results, include_answer, and (for news) topic+days, then parse the
// answer and result hits (trimming content into Snippet).
func TestSearch_ShapesBodyAndParsesResponse(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"answer":"  Go 1.26 shipped.  ","results":[
			{"title":"Go 1.26","url":"https://go.dev","content":"  notes  ","score":0.9,"published_date":"2026-07-01"}
		]}`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "tavily", APIKey: "tvly-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "tavily" {
		t.Errorf("Type() = %q, want tavily", c.Type())
	}
	resp, err := c.Search(context.Background(), websearch.SearchRequest{
		Query:     "go release",
		Count:     4,
		Topic:     "news",
		Freshness: "week",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/search" {
		t.Errorf("path = %q, want /search", gotPath)
	}
	if gotBody["api_key"] != "tvly-key" {
		t.Errorf("api_key = %v, want tvly-key", gotBody["api_key"])
	}
	if gotBody["query"] != "go release" {
		t.Errorf("query = %v, want 'go release'", gotBody["query"])
	}
	if mr, _ := gotBody["max_results"].(float64); mr != 4 {
		t.Errorf("max_results = %v, want 4", gotBody["max_results"])
	}
	if ia, _ := gotBody["include_answer"].(bool); !ia {
		t.Error("include_answer must be true")
	}
	if gotBody["topic"] != "news" {
		t.Errorf("topic = %v, want news", gotBody["topic"])
	}
	if days, _ := gotBody["days"].(float64); days != 7 {
		t.Errorf("days = %v, want 7 (week)", gotBody["days"])
	}

	if resp.Answer != "Go 1.26 shipped." {
		t.Errorf("Answer should be trimmed, got %q", resp.Answer)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.Title != "Go 1.26" || r.URL != "https://go.dev" || r.Published != "2026-07-01" {
		t.Errorf("result fields wrong: %+v", r)
	}
	if r.Snippet != "notes" {
		t.Errorf("snippet should be trimmed content, got %q", r.Snippet)
	}
}

// A general (non-news) search omits topic and days from the body.
func TestSearch_GeneralOmitsNewsFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"answer":"","results":[]}`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "tavily", APIKey: "tvly-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if _, err := c.Search(context.Background(), websearch.SearchRequest{Query: "x", Count: 3}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, ok := gotBody["topic"]; ok {
		t.Error("general search must omit topic")
	}
	if _, ok := gotBody["days"]; ok {
		t.Error("general search must omit days")
	}
}

// A missing key is rejected before any network call.
func TestSearch_NoKeyRejected(t *testing.T) {
	c := New(websearch.Options{Name: "tavily"})
	if _, err := c.Search(context.Background(), websearch.SearchRequest{Query: "x"}); err == nil {
		t.Error("want error when no API key configured")
	}
}

// Rejected key and rate limit surface as distinct errors.
func TestSearch_ErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		substr string
	}{
		{http.StatusUnauthorized, "rejected the API key"},
		{http.StatusTooManyRequests, "rate limit"},
		{http.StatusInternalServerError, "HTTP 500"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := New(websearch.Options{Name: "tavily", APIKey: "tvly-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
		_, err := c.Search(context.Background(), websearch.SearchRequest{Query: "x", Count: 3})
		if err == nil {
			t.Errorf("status %d: want an error", tc.status)
		} else if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("status %d: error %q should contain %q", tc.status, err.Error(), tc.substr)
		}
		srv.Close()
	}
}

func TestFreshnessDays(t *testing.T) {
	cases := map[string]int{"day": 1, "week": 7, "month": 30, "year": 365, "": 0, "bogus": 0}
	for in, want := range cases {
		if got := freshnessDays(in); got != want {
			t.Errorf("freshnessDays(%q) = %d, want %d", in, got, want)
		}
	}
}
