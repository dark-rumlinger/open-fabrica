package pexels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/stocksearch"
)

// A search must GET /search with the key in a bare Authorization header (no
// Bearer), the query/per_page/orientation params set, then parse photos and
// resolve the requested size's URL. SourceID is the int id as a string.
func TestSearch_ShapesRequestAndParses(t *testing.T) {
	var gotPath, gotAuth, gotQuery, gotPerPage, gotOrientation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("query")
		gotPerPage = r.URL.Query().Get("per_page")
		gotOrientation = r.URL.Query().Get("orientation")
		w.Write([]byte(`{"photos":[
			{"id":123,"width":4000,"height":3000,"photographer":"Ada","alt":"a bridge",
			 "src":{"original":"http://img/orig.jpg","large":"http://img/large.jpg","medium":"http://img/med.jpg"}}
		]}`))
	}))
	defer srv.Close()

	c := New(stocksearch.Options{Name: "pexels", APIKey: "pexels-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "pexels" {
		t.Errorf("Type() = %q, want pexels", c.Type())
	}
	resp, err := c.Search(context.Background(), stocksearch.SearchRequest{
		Query:       "bridge",
		Count:       10,
		Orientation: "landscape",
		PhotoSize:   "original",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/search" {
		t.Errorf("path = %q, want /search", gotPath)
	}
	if gotAuth != "pexels-key" {
		t.Errorf("Authorization = %q, want the bare key (no Bearer)", gotAuth)
	}
	if gotQuery != "bridge" {
		t.Errorf("query = %q, want bridge", gotQuery)
	}
	if gotPerPage != "10" {
		t.Errorf("per_page = %q, want 10", gotPerPage)
	}
	if gotOrientation != "landscape" {
		t.Errorf("orientation = %q, want landscape", gotOrientation)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.URL != "http://img/orig.jpg" {
		t.Errorf("URL = %q, want the original rendition", r.URL)
	}
	if r.Width != 4000 || r.Height != 3000 || r.Photographer != "Ada" || r.Alt != "a bridge" {
		t.Errorf("result fields wrong: %+v", r)
	}
	if r.SourceID != "123" {
		t.Errorf("SourceID = %q, want 123", r.SourceID)
	}
	if r.Attribution != "" {
		t.Errorf("Pexels requires no attribution, got %q", r.Attribution)
	}
}

// An unknown/empty PhotoSize resolves to the "large" rendition (the default).
func TestSearch_DefaultsToLargeURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"photos":[{"id":1,"src":{"large":"http://img/large.jpg","original":"http://img/orig.jpg"}}]}`))
	}))
	defer srv.Close()

	c := New(stocksearch.Options{Name: "pexels", APIKey: "pexels-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	resp, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x", Count: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Results[0].URL != "http://img/large.jpg" {
		t.Errorf("URL = %q, want the large rendition by default", resp.Results[0].URL)
	}
}

func TestSearch_NoKeyRejected(t *testing.T) {
	c := New(stocksearch.Options{Name: "pexels"})
	if _, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x"}); err == nil {
		t.Error("want error when no API key configured")
	}
}

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
		c := New(stocksearch.Options{Name: "pexels", APIKey: "pexels-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
		_, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x", Count: 3})
		if err == nil {
			t.Errorf("status %d: want an error", tc.status)
		} else if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("status %d: error %q should contain %q", tc.status, err.Error(), tc.substr)
		}
		srv.Close()
	}
}
