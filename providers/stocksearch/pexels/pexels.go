// Package pexels implements stocksearch.Provider against the Pexels v1
// search API. Generous free tier, no attribution required, key passed in
// the Authorization header (no "Bearer" prefix). Registered as type
// identifier "pexels".
package pexels

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

// defaults are Pexels' own requirements: the key goes in a bare Authorization
// header, with no "Bearer " prefix.
var defaults = stocksearch.Defaults{
	BaseURL: "https://api.pexels.com/v1",
	Auth:    providercfg.Auth{Scheme: providercfg.SchemeHeader, Header: "Authorization"},
}

type Client struct {
	*stocksearch.BaseClient
}

func init() {
	stocksearch.RegisterProvider("pexels", func(o stocksearch.Options) stocksearch.Provider {
		return New(o)
	})
}

func New(o stocksearch.Options) *Client {
	return &Client{stocksearch.NewBaseClient(o, defaults)}
}

func (c *Client) Type() string { return "pexels" }

func (c *Client) Capabilities() stocksearch.Capabilities {
	return stocksearch.Capabilities{
		MaxCount:            80, // Pexels per_page max
		Sizes:               []string{"original", "large", "medium", "small", "tiny"},
		RequiresAttribution: false,
	}
}

// pexelsResponse mirrors the Pexels /v1/search payload. Src exposes every
// rendition the API returns; we pick one based on the canonical PhotoSize
// requested by the caller.
type pexelsResponse struct {
	Photos []struct {
		ID           int    `json:"id"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		Photographer string `json:"photographer"`
		Alt          string `json:"alt"`
		Src          struct {
			Original  string `json:"original"`
			Large2x   string `json:"large2x"`
			Large     string `json:"large"`
			Medium    string `json:"medium"`
			Small     string `json:"small"`
			Portrait  string `json:"portrait"`
			Landscape string `json:"landscape"`
			Tiny      string `json:"tiny"`
		} `json:"src"`
	} `json:"photos"`
}

func (c *Client) Search(ctx context.Context, req stocksearch.SearchRequest) (*stocksearch.SearchResponse, error) {
	count, err := c.Prepare("pexels", req.Query, req.Count, 1, 6, 80)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("query", req.Query)
	q.Set("per_page", fmt.Sprintf("%d", count))
	if req.Orientation != "" {
		q.Set("orientation", req.Orientation)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("pexels: build request: %w", err)
	}
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("pexels: send: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("pexels rejected the API key (401), check it's valid")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("pexels rate limit hit (429), try again later")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pexels: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("pexels: read body: %w", err)
	}
	var parsed pexelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("pexels: decode: %w", err)
	}

	out := &stocksearch.SearchResponse{Results: make([]stocksearch.SearchResult, 0, len(parsed.Photos))}
	for _, p := range parsed.Photos {
		out.Results = append(out.Results, stocksearch.SearchResult{
			URL:          pickPexelsURL(p.Src, req.PhotoSize),
			Width:        p.Width,
			Height:       p.Height,
			Photographer: p.Photographer,
			Alt:          p.Alt,
			SourceID:     fmt.Sprintf("%d", p.ID),
		})
	}
	return out, nil
}

// pickPexelsURL maps a canonical PhotoSize token to the matching field on
// the Pexels Src struct. Falls back to "large" when the requested size is
// empty or unknown, that's the historical default and a reasonable
// middle-ground for page assets.
func pickPexelsURL(src struct {
	Original  string `json:"original"`
	Large2x   string `json:"large2x"`
	Large     string `json:"large"`
	Medium    string `json:"medium"`
	Small     string `json:"small"`
	Portrait  string `json:"portrait"`
	Landscape string `json:"landscape"`
	Tiny      string `json:"tiny"`
}, size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "original":
		return src.Original
	case "medium":
		return src.Medium
	case "small":
		return src.Small
	case "tiny":
		return src.Tiny
	}
	return src.Large
}
