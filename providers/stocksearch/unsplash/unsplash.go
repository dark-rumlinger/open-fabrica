// Package unsplash implements stocksearch.Provider against the Unsplash
// search API. Free tier is 50 req/hr (5000/hr after production approval).
// Auth: Authorization header with "Client-ID <access_key>" prefix.
// Attribution is required by the Unsplash API guidelines, the adapter
// pre-formats a credit line into SearchResult.Attribution. Registered as
// type identifier "unsplash".
package unsplash

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

// defaults are Unsplash's own requirements: the auth header format is exactly
// "Client-ID <access_key>", and the API version must be pinned.
var defaults = stocksearch.Defaults{
	BaseURL: "https://api.unsplash.com",
	Auth: providercfg.Auth{
		Scheme: providercfg.SchemeHeader,
		Header: "Authorization",
		Prefix: "Client-ID ",
	},
	Headers: map[string]string{"Accept-Version": "v1"},
}

type Client struct {
	*stocksearch.BaseClient
}

func init() {
	stocksearch.RegisterProvider("unsplash", func(o stocksearch.Options) stocksearch.Provider {
		return New(o)
	})
}

func New(o stocksearch.Options) *Client {
	return &Client{stocksearch.NewBaseClient(o, defaults)}
}

func (c *Client) Type() string { return "unsplash" }

func (c *Client) Capabilities() stocksearch.Capabilities {
	return stocksearch.Capabilities{
		MaxCount:            30, // Unsplash search per_page max
		Sizes:               []string{"original", "large", "medium", "small", "tiny"},
		RequiresAttribution: true,
	}
}

// unsplashResponse mirrors the relevant subset of /search/photos. URLs
// expose multiple renditions; we pick one via pickUnsplashURL.
type unsplashResponse struct {
	Results []struct {
		ID          string `json:"id"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Description string `json:"description"`
		AltDesc     string `json:"alt_description"`
		URLs        struct {
			Raw     string `json:"raw"`
			Full    string `json:"full"`
			Regular string `json:"regular"`
			Small   string `json:"small"`
			Thumb   string `json:"thumb"`
		} `json:"urls"`
		User struct {
			Name string `json:"name"`
		} `json:"user"`
	} `json:"results"`
}

func (c *Client) Search(ctx context.Context, req stocksearch.SearchRequest) (*stocksearch.SearchResponse, error) {
	count, err := c.Prepare("unsplash", req.Query, req.Count, 1, 6, 30)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("query", req.Query)
	q.Set("per_page", fmt.Sprintf("%d", count))
	if o := mapOrientation(req.Orientation); o != "" {
		q.Set("orientation", o)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/search/photos?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("unsplash: build request: %w", err)
	}
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("unsplash: send: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("unsplash rejected the API key (401), confirm it's an Access Key, not a Secret Key")
	case http.StatusForbidden:
		return nil, fmt.Errorf("unsplash 403, likely rate-limited (50/hr free tier)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unsplash: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("unsplash: read body: %w", err)
	}
	var parsed unsplashResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unsplash: decode: %w", err)
	}

	out := &stocksearch.SearchResponse{Results: make([]stocksearch.SearchResult, 0, len(parsed.Results))}
	for _, p := range parsed.Results {
		alt := p.AltDesc
		if alt == "" {
			alt = p.Description
		}
		photographer := p.User.Name
		out.Results = append(out.Results, stocksearch.SearchResult{
			URL:          pickUnsplashURL(p.URLs, req.PhotoSize),
			Width:        p.Width,
			Height:       p.Height,
			Photographer: photographer,
			Alt:          alt,
			SourceID:     p.ID,
			// Unsplash attribution policy: credit the photographer and link
			// to Unsplash. We pre-format the credit string here so callers
			// don't have to know the policy themselves.
			Attribution: fmt.Sprintf("Photo by %s on Unsplash", photographer),
		})
	}
	return out, nil
}

// mapOrientation translates our canonical tokens to Unsplash's vocabulary.
// Unsplash uses "landscape" / "portrait" / "squarish", note the 'ish'.
// Empty / unknown maps to empty (no orientation filter).
func mapOrientation(o string) string {
	switch strings.ToLower(strings.TrimSpace(o)) {
	case "landscape":
		return "landscape"
	case "portrait":
		return "portrait"
	case "square":
		return "squarish"
	}
	return ""
}

// pickUnsplashURL maps the canonical PhotoSize tokens to Unsplash's URL
// fields. Unsplash's "regular" is ~1080w which is a sensible "large"
// equivalent; "full" is the original aspect at higher resolution; "raw"
// is unprocessed and supports dynamic resizing via query params.
func pickUnsplashURL(urls struct {
	Raw     string `json:"raw"`
	Full    string `json:"full"`
	Regular string `json:"regular"`
	Small   string `json:"small"`
	Thumb   string `json:"thumb"`
}, size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "original":
		return urls.Full
	case "medium":
		// Unsplash has no "medium", Regular (~1080w) is the closest
		// commonly-served size. Same as our default for "large".
		return urls.Regular
	case "small":
		return urls.Small
	case "tiny":
		return urls.Thumb
	}
	return urls.Regular
}
