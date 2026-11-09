package pixabay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/stocksearch"
)

// A search must GET /?key=...&q=...&per_page=...&safesearch=true with the key
// as a query param (not a header), then parse hits and resolve the requested
// size's URL.
func TestSearch_ShapesRequestAndParses(t *testing.T) {
	var gotPath, gotKey, gotQuery, gotSafe, gotOrientation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.URL.Query().Get("key")
		gotQuery = r.URL.Query().Get("q")
		gotSafe = r.URL.Query().Get("safesearch")
		gotOrientation = r.URL.Query().Get("orientation")
		w.Write([]byte(`{"hits":[
			{"id":42,"tags":"cat, pet","user":"Bob","imageWidth":1920,"imageHeight":1080,
			 "previewURL":"http://img/prev.jpg","webformatURL":"http://img/web.jpg","largeImageURL":"http://img/large.jpg"}
		]}`))
	}))
	defer srv.Close()

	c := New(stocksearch.Options{Name: "pixabay", APIKey: "pix-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "pixabay" {
		t.Errorf("Type() = %q, want pixabay", c.Type())
	}
	resp, err := c.Search(context.Background(), stocksearch.SearchRequest{
		Query:       "cat",
		Count:       5,
		Orientation: "portrait",
		PhotoSize:   "medium",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/" {
		t.Errorf("path = %q, want /", gotPath)
	}
	if gotKey != "pix-key" {
		t.Errorf("key param = %q, want pix-key", gotKey)
	}
	if gotQuery != "cat" {
		t.Errorf("q = %q, want cat", gotQuery)
	}
	if gotSafe != "true" {
		t.Errorf("safesearch = %q, want true", gotSafe)
	}
	if gotOrientation != "vertical" {
		t.Errorf("orientation = %q, want vertical (from portrait)", gotOrientation)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.URL != "http://img/web.jpg" {
		t.Errorf("URL = %q, want the webformat (medium) rendition", r.URL)
	}
	if r.Width != 1920 || r.Height != 1080 || r.Photographer != "Bob" || r.Alt != "cat, pet" {
		t.Errorf("result fields wrong: %+v", r)
	}
	if r.SourceID != "42" {
		t.Errorf("SourceID = %q, want 42", r.SourceID)
	}
}

// Pixabay's per_page floor is 3, so a request for fewer over-fetches on the
// wire but the client trims the returned results back to the caller's Count.
func TestSearch_TrimsOverfetchToRequestedCount(t *testing.T) {
	var gotPerPage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPerPage = r.URL.Query().Get("per_page")
		w.Write([]byte(`{"hits":[
			{"id":1,"largeImageURL":"a"},{"id":2,"largeImageURL":"b"},{"id":3,"largeImageURL":"c"}
		]}`))
	}))
	defer srv.Close()

	c := New(stocksearch.Options{Name: "pixabay", APIKey: "pix-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	resp, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x", Count: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPerPage != "3" {
		t.Errorf("per_page = %q, want 3 (the API floor)", gotPerPage)
	}
	if len(resp.Results) != 2 {
		t.Errorf("results should be trimmed to the requested count 2, got %d", len(resp.Results))
	}
}

func TestSearch_NoKeyRejected(t *testing.T) {
	c := New(stocksearch.Options{Name: "pixabay"})
	if _, err := c.Search(context.Background(), stocksearch.SearchRequest{Query: "x"}); err == nil {
		t.Error("want error when no API key configured")
	}
}

// A 400 (bad key/query), a 429, and a generic non-200 surface as distinct errors.
func TestSearch_ErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		substr string
	}{
		{http.StatusBadRequest, "[ERROR] invalid key", "pixabay 400"},
		{http.StatusTooManyRequests, "", "rate limit"},
		{http.StatusInternalServerError, "", "HTTP 500"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		c := New(stocksearch.Options{Name: "pixabay", APIKey: "pix-key", BaseURL: srv.URL, HTTPClient: srv.Client()})
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
	cases := map[string]string{"landscape": "horizontal", "portrait": "vertical", "square": "", "": ""}
	for in, want := range cases {
		if got := mapOrientation(in); got != want {
			t.Errorf("mapOrientation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickPixabayURL(t *testing.T) {
	large, web, preview := "L", "W", "P"
	cases := map[string]string{"small": "P", "tiny": "P", "medium": "W", "large": "L", "original": "L", "": "L"}
	for size, want := range cases {
		if got := pickPixabayURL(large, web, preview, size); got != want {
			t.Errorf("pickPixabayURL size=%q = %q, want %q", size, got, want)
		}
	}
}
