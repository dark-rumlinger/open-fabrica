package duckduckgo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openfabrica/providers/websearch"
)

func TestSearch_ParsesHTMLAndUnwrapsRedirects(t *testing.T) {
	var gotMethod, gotQuery, gotDF, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotUA = r.Header.Get("User-Agent")
		_ = r.ParseForm()
		gotQuery = r.FormValue("q")
		gotDF = r.FormValue("df")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
		<div class="results">
			<div class="result result--ad">
				<a class="result__a" href="https://ads.example.com">Sponsored Ad</a>
				<a class="result__snippet">Buy something</a>
			</div>
			<div class="result results_links">
				<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=abc">Go Documentation</a>
				<a class="result__snippet">Official   <b>Go</b>   docs and tutorials.</a>
			</div>
			<div class="result results_links">
				<a class="result__a" href="https://pkg.go.dev/net/http">net/http package</a>
				<div class="result__snippet">HTTP client and server implementations.</div>
			</div>
		</div>
		</body></html>`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "duckduckgo", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "duckduckgo" {
		t.Fatalf("Type() = %q, want duckduckgo", c.Type())
	}

	resp, err := c.Search(context.Background(), websearch.SearchRequest{
		Query:     "go http docs",
		Count:     5,
		Freshness: "month",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotQuery != "go http docs" {
		t.Errorf("q = %q, want 'go http docs'", gotQuery)
	}
	if gotDF != "m" {
		t.Errorf("df = %q, want m", gotDF)
	}
	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("User-Agent = %q, expected browser UA", gotUA)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("got %d results, want 2 (ad skipped)", len(resp.Results))
	}
	if resp.Results[0].Title != "Go Documentation" || resp.Results[0].URL != "https://go.dev/doc/" {
		t.Errorf("result[0] = %+v", resp.Results[0])
	}
	if resp.Results[0].Snippet != "Official Go docs and tutorials." {
		t.Errorf("result[0].Snippet = %q", resp.Results[0].Snippet)
	}
	if resp.Results[1].URL != "https://pkg.go.dev/net/http" {
		t.Errorf("result[1].URL = %q", resp.Results[1].URL)
	}
}

func TestSearch_CacheHitsAvoidDuplicateRequests(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`<div class="result"><a class="result__a" href="https://example.com">Example</a><a class="result__snippet">Snippet</a></div>`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "duckduckgo", BaseURL: srv.URL, HTTPClient: srv.Client()})
	for i := 0; i < 2; i++ {
		resp, err := c.Search(context.Background(), websearch.SearchRequest{Query: "cached query", Count: 3})
		if err != nil {
			t.Fatalf("Search %d: %v", i, err)
		}
		if len(resp.Results) != 1 {
			t.Fatalf("Search %d: got %d results", i, len(resp.Results))
		}
	}
	if hits.Load() != 1 {
		t.Errorf("server hit %d times, want 1 (second should be cached)", hits.Load())
	}
}

func TestSearch_ChallengePageReturnsErrorAndTriggersCooldown(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`<div class="anomaly-modal">Unfortunately, bots use DuckDuckGo too.</div>`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "duckduckgo", BaseURL: srv.URL, HTTPClient: srv.Client()})
	c.minInterval = 0
	c.cooldown = 5 * time.Second

	_, err := c.Search(context.Background(), websearch.SearchRequest{Query: "blocked", Count: 3})
	if err == nil {
		t.Fatal("expected error on challenge page, got nil")
	}
	if !strings.Contains(err.Error(), "challenge") {
		t.Errorf("unexpected error: %v", err)
	}

	// Second request during cooldown should fail fast without hitting the server.
	_, err = c.Search(context.Background(), websearch.SearchRequest{Query: "blocked-2", Count: 3})
	if err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("expected cooldown error, got: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hit %d times, want 1 (cooldown should block second request)", hits.Load())
	}
}

func TestSearch_PacingSpacesOutRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div class="result"><a class="result__a" href="https://example.com">Example</a></div>`))
	}))
	defer srv.Close()

	c := New(websearch.Options{Name: "duckduckgo", BaseURL: srv.URL, HTTPClient: srv.Client()})
	c.minInterval = 30 * time.Millisecond

	start := time.Now()
	if _, err := c.Search(context.Background(), websearch.SearchRequest{Query: "q1", Count: 1}); err != nil {
		t.Fatalf("Search 1: %v", err)
	}
	if _, err := c.Search(context.Background(), websearch.SearchRequest{Query: "q2", Count: 1}); err != nil {
		t.Fatalf("Search 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Errorf("elapsed = %v, expected >= 25ms pacing between requests", elapsed)
	}
}
