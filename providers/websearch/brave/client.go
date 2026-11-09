// Package brave implements websearch.Provider against the Brave Search
// API. Broad open-web coverage with a generous free tier; the key is
// passed in the X-Subscription-Token header. Brave returns description
// snippets and per-result age strings but no synthesized answer.
// Registered as type identifier "brave".
package brave

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/websearch"
)

// defaults are Brave's own requirements: the key goes in X-Subscription-Token.
var defaults = websearch.Defaults{
	BaseURL: "https://api.search.brave.com/res/v1",
	Auth:    providercfg.Auth{Scheme: providercfg.SchemeHeader, Header: "X-Subscription-Token"},
}

type Client struct {
	*websearch.BaseClient
}

func init() {
	websearch.RegisterProvider("brave", func(o websearch.Options) websearch.Provider {
		return New(o)
	})
}

func New(o websearch.Options) *Client {
	return &Client{websearch.NewBaseClient(o, defaults)}
}

func (c *Client) Type() string { return "brave" }

func (c *Client) Capabilities() websearch.Capabilities {
	return websearch.Capabilities{
		MaxCount:       20, // Brave `count` ceiling
		SupportsAnswer: false,
		SupportsNews:   true,
	}
}

// braveWebResponse mirrors the subset of the /web/search payload we use.
// The news endpoint shares the {results:[{title,url,description,age}]}
// shape, so one struct decodes both.
type braveWebResponse struct {
	Web struct {
		Results []braveResult `json:"results"`
	} `json:"web"`
	Results []braveResult `json:"results"` // news endpoint top-level
}

type braveResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Age         string `json:"age"`      // e.g. "2 days ago"
	PageAge     string `json:"page_age"` // ISO timestamp when present
}

// freshnessParam maps the canonical freshness token to Brave's `freshness`
// values (pd=past day, pw=week, pm=month, py=year). Empty means no filter.
func freshnessParam(f string) string {
	switch f {
	case "day":
		return "pd"
	case "week":
		return "pw"
	case "month":
		return "pm"
	case "year":
		return "py"
	}
	return ""
}

func (c *Client) Search(ctx context.Context, req websearch.SearchRequest) (*websearch.SearchResponse, error) {
	count, err := c.Prepare("brave", req.Query, req.Count, 1, 6, 20)
	if err != nil {
		return nil, err
	}

	// The news endpoint surfaces recent articles; everything else uses the
	// web endpoint. Both honor `freshness`.
	path := "/web/search"
	if req.Topic == "news" {
		path = "/news/search"
	}

	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("count", fmt.Sprintf("%d", count))
	if fp := freshnessParam(req.Freshness); fp != "" {
		q.Set("freshness", fp)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("brave: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("brave: send: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("brave rejected the API key (%d), check it's valid", resp.StatusCode)
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("brave rate limit hit (429), try again later")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave: HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, fmt.Errorf("brave: read body: %w", err)
	}
	var parsed braveWebResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("brave: decode body: %w", err)
	}

	hits := parsed.Web.Results
	if len(hits) == 0 {
		hits = parsed.Results // news endpoint
	}

	out := &websearch.SearchResponse{}
	for _, r := range hits {
		published := r.PageAge
		if published == "" {
			published = r.Age
		}
		out.Results = append(out.Results, websearch.SearchResult{
			Title:     r.Title,
			URL:       r.URL,
			Snippet:   strings.TrimSpace(r.Description),
			Published: published,
		})
	}
	return out, nil
}
