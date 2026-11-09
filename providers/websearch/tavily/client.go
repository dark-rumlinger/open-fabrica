// Package tavily implements websearch.Provider against the Tavily Search
// API. Tavily is purpose-built for LLM grounding: it returns cleaned page
// extracts plus an optional synthesized answer, so the build/live model
// can cite current facts without a secondary fetch. Key is sent in the
// JSON body. Registered as type identifier "tavily".
package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/websearch"
)

// defaults are Tavily's own requirements: the key goes in the JSON body, which
// Authorize cannot inject into a typed payload - Search sets it directly, and
// the descriptor is here so nothing also sends it as a header.
var defaults = websearch.Defaults{
	BaseURL: "https://api.tavily.com",
	Auth:    providercfg.Auth{Scheme: providercfg.SchemeBody, Param: "api_key"},
}

type Client struct {
	*websearch.BaseClient
}

func init() {
	websearch.RegisterProvider("tavily", func(o websearch.Options) websearch.Provider {
		return New(o)
	})
}

func New(o websearch.Options) *Client {
	return &Client{websearch.NewBaseClient(o, defaults)}
}

func (c *Client) Type() string { return "tavily" }

func (c *Client) Capabilities() websearch.Capabilities {
	return websearch.Capabilities{
		MaxCount:       20, // Tavily max_results ceiling
		SupportsAnswer: true,
		SupportsNews:   true,
	}
}

// tavilyRequest mirrors the POST /search payload. We always ask for an
// include_answer so the model gets a grounded summary alongside the hits.
type tavilyRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results"`
	SearchDepth   string `json:"search_depth"`
	Topic         string `json:"topic,omitempty"`
	IncludeAnswer bool   `json:"include_answer"`
	Days          int    `json:"days,omitempty"`
}

type tavilyResponse struct {
	Answer  string `json:"answer"`
	Results []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date"`
	} `json:"results"`
}

// freshnessDays maps the canonical freshness token to Tavily's `days`
// window (only meaningful with topic="news"). Zero means "no window".
func freshnessDays(f string) int {
	switch f {
	case "day":
		return 1
	case "week":
		return 7
	case "month":
		return 30
	case "year":
		return 365
	}
	return 0
}

func (c *Client) Search(ctx context.Context, req websearch.SearchRequest) (*websearch.SearchResponse, error) {
	count, err := c.Prepare("tavily", req.Query, req.Count, 1, 6, 20)
	if err != nil {
		return nil, err
	}

	payload := tavilyRequest{
		APIKey:        c.APIKey(),
		Query:         req.Query,
		MaxResults:    count,
		SearchDepth:   "basic",
		IncludeAnswer: true,
	}
	if req.Topic == "news" {
		payload.Topic = "news"
		payload.Days = freshnessDays(req.Freshness)
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tavily: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL()+"/search", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("tavily: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tavily: send: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("tavily rejected the API key (%d), check it's valid", resp.StatusCode)
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("tavily rate limit hit (429), try again later")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily: HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, fmt.Errorf("tavily: read body: %w", err)
	}
	var parsed tavilyResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("tavily: decode body: %w", err)
	}

	out := &websearch.SearchResponse{Answer: strings.TrimSpace(parsed.Answer)}
	for _, r := range parsed.Results {
		out.Results = append(out.Results, websearch.SearchResult{
			Title:     r.Title,
			URL:       r.URL,
			Snippet:   strings.TrimSpace(r.Content),
			Published: r.PublishedDate,
		})
	}
	return out, nil
}
