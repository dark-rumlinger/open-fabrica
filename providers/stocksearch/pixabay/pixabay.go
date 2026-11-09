// Package pixabay implements stocksearch.Provider against the Pixabay API.
// Free tier: 100 req/min, 10k/day. Auth: API key passed as a `key=` query
// parameter (not a header). No attribution required (though appreciated).
// Registered as type identifier "pixabay".
package pixabay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/stocksearch"
)

// defaults are Pixabay's own requirements: the key rides the query string.
var defaults = stocksearch.Defaults{
	BaseURL: "https://pixabay.com/api",
	Auth:    providercfg.Auth{Scheme: providercfg.SchemeQuery, Param: "key"},
}

type Client struct {
	*stocksearch.BaseClient
}

func init() {
	stocksearch.RegisterProvider("pixabay", func(o stocksearch.Options) stocksearch.Provider {
		return New(o)
	})
}

func New(o stocksearch.Options) *Client {
	return &Client{stocksearch.NewBaseClient(o, defaults)}
}

func (c *Client) Type() string { return "pixabay" }

func (c *Client) Capabilities() stocksearch.Capabilities {
	return stocksearch.Capabilities{
		MaxCount: 200, // Pixabay per_page max
		// Pixabay exposes preview/webformat/largeImage URL fields and a
		// gated full-resolution URL only available to paying users, we
		// surface the three free ones via the canonical tokens below.
		Sizes:               []string{"large", "medium", "small"},
		RequiresAttribution: false,
	}
}

// pixabayResponse mirrors the relevant subset of the search payload.
// PreviewURL is small (~150px), WebformatURL is the standard size up to
// 640w, LargeImageURL is up to 1280w.
type pixabayResponse struct {
	Hits []struct {
		ID            int    `json:"id"`
		Tags          string `json:"tags"`
		User          string `json:"user"`
		ImageWidth    int    `json:"imageWidth"`
		ImageHeight   int    `json:"imageHeight"`
		PreviewURL    string `json:"previewURL"`
		WebformatURL  string `json:"webformatURL"`
		LargeImageURL string `json:"largeImageURL"`
	} `json:"hits"`
}

func (c *Client) Search(ctx context.Context, req stocksearch.SearchRequest) (*stocksearch.SearchResponse, error) {
	// Pixabay rejects per_page < 3, so the floor and default are both 3.
	count, err := c.Prepare("pixabay", req.Query, req.Count, 3, 3, 200)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("per_page", fmt.Sprintf("%d", count))
	q.Set("safesearch", "true")
	if o := mapOrientation(req.Orientation); o != "" {
		q.Set("orientation", o)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("pixabay: build request: %w", err)
	}
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("pixabay: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("pixabay 400, likely invalid key or query: %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("pixabay rate limit hit (429), try again later")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pixabay: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("pixabay: read body: %w", err)
	}
	var parsed pixabayResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("pixabay: decode: %w", err)
	}

	out := &stocksearch.SearchResponse{Results: make([]stocksearch.SearchResult, 0, len(parsed.Hits))}
	// Trim count down, Pixabay's per_page floor of 3 means we may have
	// over-fetched for callers asking for 1 or 2 results.
	limit := req.Count
	if limit < 1 {
		limit = len(parsed.Hits)
	}
	for i, p := range parsed.Hits {
		if i >= limit {
			break
		}
		out.Results = append(out.Results, stocksearch.SearchResult{
			URL:          pickPixabayURL(p.LargeImageURL, p.WebformatURL, p.PreviewURL, req.PhotoSize),
			Width:        p.ImageWidth,
			Height:       p.ImageHeight,
			Photographer: p.User,
			Alt:          p.Tags,
			SourceID:     fmt.Sprintf("%d", p.ID),
		})
	}
	return out, nil
}

// mapOrientation translates canonical tokens to Pixabay's vocabulary:
// "horizontal" / "vertical" / "all". "square" has no native equivalent
// , fall back to "all" and let the caller filter by aspect if needed.
func mapOrientation(o string) string {
	switch strings.ToLower(strings.TrimSpace(o)) {
	case "landscape":
		return "horizontal"
	case "portrait":
		return "vertical"
	}
	return ""
}

// pickPixabayURL maps the canonical PhotoSize tokens to Pixabay's URL
// fields. "original" / "large" -> largeImageURL (~1280w), "medium" ->
// webformatURL (~640w), "small" / "tiny" -> previewURL (~150w).
func pickPixabayURL(large, web, preview, size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "small", "tiny":
		return preview
	case "medium":
		return web
	}
	return large
}
