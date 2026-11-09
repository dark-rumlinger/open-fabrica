package unsplash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/stocksearch"
)

// A search must GET /search/photos with the key in a "Client-ID <key>"
// Authorization header plus Accept-Version: v1, then parse results, resolve the
// requested size's URL, and pre-format the required attribution line.
func TestSearch_ShapesRequestAndParses(t *testing.T) {
	var gotPath, gotAuth, gotAcceptVersion, gotQuery, gotPerPage, gotOrientation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAcceptVersion = r.Header.Get("Accept-Version")
		gotQuery = r.URL.Query().Get("query")
		gotPerPage = r.URL.Query().Get("per_page")
		gotOrientation = r.URL.Query().Get("orientation")
		w.Write([]byte(`{"results":[
			{"id":"abc123","width":5000,"height":3000,"description":"","alt_description":"a mountain",
			 "urls":{"raw":"http://img/raw","full":"http://img/full","regular":"http://img/reg","small":"http://img/sm","thumb":"http://img/th"},
			 "user":{"name":"Ansel"}}
		]}`))
	}))
	defer srv.Close()

	c := New(stocksearch.Options{Name: "unsplash", APIKey: "unsplash-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "unsplash" {
		t.Errorf("Type() = %q, want unsplash", c.Type())
	}
	resp, err := c.Search(context.Background(), stocksearch.SearchRequest{
		Query:       "mountain",
		Count:       8,
		Orientation: "square",
		PhotoSize:   "original",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/search/photos" {
		t.Errorf("path = %q, want /search/photos", gotPath)
	}
	if gotAuth != "Client-ID unsplash-key" {
		t.Errorf("Authorization = %q, want 'Client-ID unsplash-key'", gotAuth)
	}
	if gotAcceptVersion != "v1" {
		t.Errorf("Accept-Version = %q, want v1", gotAcceptVersion)
	}
	if gotQuery != "mountain" {
		t.Errorf("query = %q, want mountain", gotQuery)
	}
	if gotPerPage != "8" {
		t.Errorf("per_page = %q, want 8", gotPerPage)
	}
	if gotOrientation != "squarish" {
		t.Errorf("orientation = %q, want squarish (from square)", gotOrientation)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.URL != "http://img/full" {
		t.Errorf("URL = %q, want the full rendition for 'original'", r.URL)
	}
	if r.Width != 5000 || r.Height != 3000 || r.Photographer != "Ansel" {
		t.Errorf("result fields wrong: %+v", r)
	}
	// alt_description wins over the empty description.
	if r.Alt != "a mountain" {
		t.Errorf("Alt = %q, want 'a mountain'", r.Alt)
	}
	if r.SourceID != "abc123" {
		t.Errorf("SourceID = %q, want the slug abc123", r.SourceID)
	}
	if r.Attribution != "Photo by Ansel on Unsplash" {
		t.Errorf("Attribution = %q", r.Attribution)
	}
}

// An unknown/empty PhotoSize resolves to the "regular" rendition (the default).
func TestSearch_DefaultsToRegularURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"id":"x","urls":{"regular":"http://img/reg","full":"http://img/full"},"user":{"name":"Q"}}]}`))
	}))
	defer srv.Close()

	c := New(stocksearch.Options{Name: "unsplash", APIKey: "unsplash-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	resp, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x", Count: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Results[0].URL != "http://img/reg" {
		t.Errorf("URL = %q, want the regular rendition by default", resp.Results[0].URL)
	}
}

func TestSearch_NoKeyRejected(t *testing.T) {
	c := New(stocksearch.Options{Name: "unsplash"})
	if _, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x"}); err == nil {
		t.Error("want error when no API key configured")
	}
}

// A rejected key (401) and a rate limit (403) surface as distinct errors.
func TestSearch_ErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		substr string
	}{
		{http.StatusUnauthorized, "rejected the API key"},
		{http.StatusForbidden, "rate-limited"},
		{http.StatusInternalServerError, "HTTP 500"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := New(stocksearch.Options{Name: "unsplash", APIKey: "unsplash-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
		_, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x", Count: 3})
		if err == nil {
			t.Errorf("status %d: want an error", tc.status)
		} else if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("status %d: error %q should contain %q", tc.status, err.Error(), tc.substr)
		}
		srv.Close()
	}
}

func TestMapOrientation(t *testing.T) {
	cases := map[string]string{"landscape": "landscape", "portrait": "portrait", "square": "squarish", "": ""}
	for in, want := range cases {
		if got := mapOrientation(in); got != want {
			t.Errorf("mapOrientation(%q) = %q, want %q", in, got, want)
		}
	}
}
