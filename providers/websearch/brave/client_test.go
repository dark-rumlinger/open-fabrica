package brave

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/websearch"
)

// A web search must GET /web/search with the key in X-Subscription-Token and
// the query/count/freshness mapped into query params, then parse web.results
// (preferring page_age over age for Published).
func TestSearch_WebEndpointShapeAndParse(t *testing.T) {
	var gotPath, gotToken string
	var gotQuery, gotCount, gotFreshness string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		gotFreshness = r.URL.Query().Get("freshness")
		w.Write([]byte(`{"web":{"results":[
			{"title":"Go 1.26","url":"https://go.dev","description":"  release notes  ","age":"2 days ago","page_age":"2026-07-01T00:00:00Z"}
		]}}`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "brave", APIKey: "brave-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "brave" {
		t.Errorf("Type() = %q, want brave", c.Type())
	}
	resp, err := c.Search(context.Background(), websearch.SearchRequest{
		Query:     "go release",
		Count:     5,
		Freshness: "week",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/web/search" {
		t.Errorf("path = %q, want /web/search", gotPath)
	}
	if gotToken != "brave-key" {
		t.Errorf("X-Subscription-Token = %q, want brave-key", gotToken)
	}
	if gotQuery != "go release" {
		t.Errorf("q = %q, want 'go release'", gotQuery)
	}
	if gotCount != "5" {
		t.Errorf("count = %q, want 5", gotCount)
	}
	if gotFreshness != "pw" {
		t.Errorf("freshness = %q, want pw (week)", gotFreshness)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.Title != "Go 1.26" || r.URL != "https://go.dev" {
		t.Errorf("result fields wrong: %+v", r)
	}
	if r.Snippet != "release notes" {
		t.Errorf("snippet should be trimmed, got %q", r.Snippet)
	}
	if r.Published != "2026-07-01T00:00:00Z" {
		t.Errorf("Published should prefer page_age, got %q", r.Published)
	}
}

// Topic="news" routes to /news/search, whose payload puts results at the top
// level; the client falls back to that when web.results is empty.
func TestSearch_NewsEndpointTopLevelResults(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"results":[{"title":"Headline","url":"https://news.example","description":"story","age":"1 hour ago"}]}`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "brave", APIKey: "brave-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	resp, err := c.Search(context.Background(), websearch.SearchRequest{Query: "today", Count: 3, Topic: "news"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/news/search" {
		t.Errorf("path = %q, want /news/search", gotPath)
	}
	if len(resp.Results) != 1 || resp.Results[0].Title != "Headline" {
		t.Fatalf("news results not parsed: %+v", resp.Results)
	}
	// With no page_age, Published falls back to the age string.
	if resp.Results[0].Published != "1 hour ago" {
		t.Errorf("Published should fall back to age, got %q", resp.Results[0].Published)
	}
}

// A missing key is rejected up-front by the shared Prepare preamble, before any
// network call.
func TestSearch_NoKeyRejected(t *testing.T) {
	c := New(websearch.Options{Name: "brave"})
	if _, err := c.Search(context.Background(), websearch.SearchRequest{Query: "x"}); err == nil {
		t.Error("want error when no API key configured")
	}
}

// A rejected key (401/403) and a rate limit (429) surface as distinct errors.
func TestSearch_ErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		substr string
	}{
		{http.StatusUnauthorized, "rejected the API key"},
		{http.StatusForbidden, "rejected the API key"},
		{http.StatusTooManyRequests, "rate limit"},
		{http.StatusInternalServerError, "HTTP 500"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := New(websearch.Options{Name: "brave", APIKey: "brave-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
		_, err := c.Search(context.Background(), websearch.SearchRequest{Query: "x", Count: 3})
		if err == nil {
			t.Errorf("status %d: want an error", tc.status)
		} else if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("status %d: error %q should contain %q", tc.status, err.Error(), tc.substr)
		}
		srv.Close()
	}
}

func TestFreshnessParam(t *testing.T) {
	cases := map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py", "": "", "bogus": ""}
	for in, want := range cases {
		if got := freshnessParam(in); got != want {
			t.Errorf("freshnessParam(%q) = %q, want %q", in, got, want)
		}
	}
}
