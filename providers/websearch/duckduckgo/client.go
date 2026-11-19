// Package duckduckgo implements websearch.Provider against DuckDuckGo's HTML
// endpoint (https://html.duckduckgo.com/html/). It requires no API key or
// external service, making it the zero-config single-binary search backend.
// Registered as type identifier "duckduckgo".
package duckduckgo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/websearch"
)

const (
	defaultUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/158.0.0.0 Safari/537.36"
	defaultSecCHUA    = `"Google Chrome";v="158", "Chromium";v="158", "Not_A Brand";v="24"`
	maxResponseBytes  = 2 << 20 // 2 MiB cap for DDG HTML pages
	cacheTTL          = 10 * time.Minute
	maxCacheEntries   = 128
	defaultMinSpacing = 1500 * time.Millisecond
	defaultCooldown   = 30 * time.Second
	maxCooldown       = 5 * time.Minute
)

var defaults = websearch.Defaults{
	BaseURL: "https://html.duckduckgo.com/html/",
	Auth:    providercfg.Auth{Scheme: providercfg.SchemeNone},
}

type cacheEntry struct {
	resp      *websearch.SearchResponse
	expiresAt time.Time
}

type Client struct {
	*websearch.BaseClient

	mu            sync.Mutex
	cache         map[string]cacheEntry
	minInterval   time.Duration
	cooldown      time.Duration
	lastReqAt     time.Time
	cooldownUntil time.Time
}

func init() {
	websearch.RegisterProvider("duckduckgo", func(o websearch.Options) websearch.Provider {
		return New(o)
	})
}

func New(o websearch.Options) *Client {
	return &Client{
		BaseClient:  websearch.NewBaseClient(o, defaults),
		cache:       make(map[string]cacheEntry),
		minInterval: defaultMinSpacing,
		cooldown:    defaultCooldown,
	}
}

func (c *Client) Type() string { return "duckduckgo" }

func (c *Client) Capabilities() websearch.Capabilities {
	return websearch.Capabilities{
		MaxCount:       20,
		SupportsAnswer: false,
		SupportsNews:   false,
	}
}

func freshnessParam(f string) string {
	switch f {
	case "day":
		return "d"
	case "week":
		return "w"
	case "month":
		return "m"
	case "year":
		return "y"
	}
	return ""
}

func prepareCount(query string, count, lo, def, max int) (int, error) {
	if strings.TrimSpace(query) == "" {
		return 0, fmt.Errorf("duckduckgo: query is required")
	}
	if count < lo {
		count = def
	}
	if count > max {
		count = max
	}
	return count, nil
}

func (c *Client) Search(ctx context.Context, req websearch.SearchRequest) (*websearch.SearchResponse, error) {
	count, err := prepareCount(req.Query, req.Count, 1, 6, 20)
	if err != nil {
		return nil, err
	}

	df := freshnessParam(req.Freshness)
	cacheKey := fmt.Sprintf("%s|%s|%d|%s", c.BaseURL(), strings.TrimSpace(req.Query), count, df)
	if cached := c.getCached(cacheKey); cached != nil {
		return cached, nil
	}

	if err := c.waitTurn(ctx); err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("q", req.Query)
	if df != "" {
		form.Set("df", df)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")
	httpReq.Header.Set("User-Agent", defaultUserAgent)
	httpReq.Header.Set("Sec-CH-UA", defaultSecCHUA)
	httpReq.Header.Set("Sec-CH-UA-Mobile", "?0")
	httpReq.Header.Set("Sec-CH-UA-Platform", `"Windows"`)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: send: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: read response: %w", err)
	}
	body := string(bodyBytes)

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || isChallengePage(body) {
		c.enterCooldown(resp.Header.Get("Retry-After"))
		return nil, fmt.Errorf("duckduckgo returned a bot/rate-limit challenge (HTTP %d); retry later or configure another search provider", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("duckduckgo returned HTTP %d", resp.StatusCode)
	}

	results, err := parseHTMLResults(body, count)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: parse html: %w", err)
	}

	out := &websearch.SearchResponse{Results: results}
	c.putCached(cacheKey, out)
	return out, nil
}

func (c *Client) waitTurn(ctx context.Context) error {
	for {
		c.mu.Lock()
		now := time.Now()
		if !c.cooldownUntil.IsZero() && now.Before(c.cooldownUntil) {
			rem := c.cooldownUntil.Sub(now).Round(time.Second)
			if rem < time.Second {
				rem = time.Second
			}
			c.mu.Unlock()
			return fmt.Errorf("duckduckgo is in rate-limit cooldown for %s; retry later or configure another search provider", rem)
		}
		var wait time.Duration
		if c.minInterval > 0 && !c.lastReqAt.IsZero() {
			if next := c.lastReqAt.Add(c.minInterval); next.After(now) {
				wait = next.Sub(now)
			}
		}
		if wait == 0 {
			c.lastReqAt = now
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("duckduckgo: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) enterCooldown(retryAfterHeader string) {
	dur := parseRetryAfter(retryAfterHeader, c.cooldown)
	if dur <= 0 {
		return
	}
	c.mu.Lock()
	c.cooldownUntil = time.Now().Add(dur)
	c.mu.Unlock()
}

func parseRetryAfter(header string, fallback time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(header); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > maxCooldown {
			return maxCooldown
		}
		return d
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			if d > maxCooldown {
				return maxCooldown
			}
			return d
		}
	}
	return fallback
}

func (c *Client) getCached(key string) *websearch.SearchResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.cache[key]
	if !ok {
		return nil
	}
	if time.Now().After(ent.expiresAt) {
		delete(c.cache, key)
		return nil
	}
	return ent.resp
}

func (c *Client) putCached(key string, resp *websearch.SearchResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxCacheEntries {
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.expiresAt) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= maxCacheEntries {
			for k := range c.cache {
				delete(c.cache, k)
				break
			}
		}
	}
	c.cache[key] = cacheEntry{resp: resp, expiresAt: time.Now().Add(cacheTTL)}
}

func isChallengePage(body string) bool {
	return strings.Contains(body, "anomaly-modal") ||
		strings.Contains(body, "bots use DuckDuckGo") ||
		strings.Contains(body, "challenge-form")
}

func parseHTMLResults(rawHTML string, limit int) ([]websearch.SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil, err
	}

	var results []websearch.SearchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(results) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "result") && !hasClass(n, "result--ad") && !hasClass(n, "result--no-result") {
			if r, ok := extractResult(n); ok {
				results = append(results, r)
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return results, nil
}

func extractResult(container *html.Node) (websearch.SearchResult, bool) {
	var title, href, snippet string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "a" && hasClass(n, "result__a") && title == "" {
				title = collapseSpace(nodeText(n))
				href = unwrapDDGURL(attrVal(n, "href"))
			} else if hasClass(n, "result__snippet") && snippet == "" {
				snippet = collapseSpace(nodeText(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(container)

	if title == "" || href == "" {
		return websearch.SearchResult{}, false
	}
	return websearch.SearchResult{
		Title:   title,
		URL:     href,
		Snippet: snippet,
	}, true
}

// unwrapDDGURL resolves DuckDuckGo's /l/?uddg=<encoded> redirect links to the
// underlying destination URL.
func unwrapDDGURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if uddg := u.Query().Get("uddg"); uddg != "" {
		return uddg
	}
	return raw
}

func hasClass(n *html.Node, target string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, cls := range strings.Fields(a.Val) {
				if cls == target {
					return true
				}
			}
		}
	}
	return false
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(nodeText(c))
	}
	return sb.String()
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
