package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/internal/stringutil"
	"openfabrica/tools"

	"golang.org/x/net/html"
)

// HTTPBodyCap bounds how much of an external response body is kept; results
// past the cap are sliced and flagged with body_truncated. extractTextCap
// bounds the plain text produced by strip_html the same way.
//
// HTTPResultBodyCap bounds the body a RESULT carries: the transcript cap would otherwise
// clip it mid-JSON with no flag. Feeds and strip_html still parse from the full read.
const (
	HTTPBodyCap       = 100 * 1024
	extractTextCap    = 50000
	HTTPResultBodyCap = 8000
)

// ClipResultBody trims a string body to HTTPResultBodyCap, reporting whether it did.
func ClipResultBody(body string) (string, bool) {
	if len(body) <= HTTPResultBodyCap {
		return body, false
	}
	return body[:HTTPResultBodyCap], true
}

// httpExecute takes typed args straight from HTTPRequest. requestBody is the
// raw interface{} arg (string, object, or array); headers is the declared
// map; an empty method falls back to GET and a zero timeout to the 30s default.
func httpExecute(ctx *tools.ToolContext, url, method string, requestBody any, headersArg map[string]any, timeoutSeconds int, stripHTML, parseFeed bool) (*tools.Result, error) {
	if url == "" {
		return tools.Fail("url is required"), nil
	}

	// Resolve relative URLs (e.g. /api/drawings) against the project's own address.
	// These are always local project URLs, so skip SSRF validation for them.
	isLocal := strings.HasPrefix(url, "/")
	if isLocal {
		url = resolveLocalURL(ctx, url)
	}

	// Detect requests to the project's own domain and route them locally.
	// The LLM sometimes uses the full domain URL instead of a relative path.
	if !isLocal {
		if ownDomain := getProjectDomain(ctx); ownDomain != "" {
			if parsed, err := neturl.Parse(url); err == nil && strings.EqualFold(parsed.Hostname(), ownDomain) {
				isLocal = true
				url = localBaseURL(ctx) + parsed.RequestURI()
			}
		}
	}

	// Block SSRF: reject non-http(s) schemes and private/internal IPs for
	// absolute URLs (which come from LLM output and are untrusted).
	if !isLocal {
		if err := netguard.ValidateExternalURL(url); err != nil {
			return tools.Fail("blocked URL: %v", err), nil
		}
	}

	if method == "" {
		method = "GET"
	}

	// Body may be a raw string or a JSON-serializable object. Track whether we
	// auto-encoded an object so we can default Content-Type below without
	// stomping a caller-provided header.
	var bodyReader io.Reader
	autoJSON := false
	switch body := requestBody.(type) {
	case string:
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
	case map[string]any:
		if len(body) > 0 {
			encoded, err := json.Marshal(body)
			if err != nil {
				return tools.Fail("encoding body: %v", err), nil
			}
			bodyReader = strings.NewReader(string(encoded))
			autoJSON = true
		}
	case []any:
		encoded, err := json.Marshal(body)
		if err != nil {
			return tools.Fail("encoding body: %v", err), nil
		}
		bodyReader = strings.NewReader(string(encoded))
		autoJSON = true
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return tools.Fail("creating request: %v", err), nil
	}

	// Set headers.
	for k, v := range headersArg {
		if vs, ok := v.(string); ok {
			req.Header.Set(k, vs)
		}
	}
	if autoJSON && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// Resolve per-request timeout: the arg's max tag caps it at 60s; a zero or
	// negative value falls back to the 30s default rather than disabling the
	// timeout.
	timeout := 30 * time.Second
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}

	client := ssrfSafeClient(isLocal)
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return tools.Fail("executing request: %v", err), nil
	}
	defer resp.Body.Close()

	// Read response body, capped at 100KB. Reading one byte past the cap
	// distinguishes "fit exactly" from "trimmed", so the result can carry an
	// explicit truncation flag instead of silently clipping.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, HTTPBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	bodyTruncated := false
	if len(respBody) > HTTPBodyCap {
		respBody = respBody[:HTTPBodyCap]
		bodyTruncated = true
	}

	// RSS/Atom feed parsing.
	if parseFeed {
		feed, err := parseRSSOrAtomFeed(respBody)
		if err != nil {
			if bodyTruncated {
				return tools.Fail("feed parse error: %v (response exceeded the 100 KB body cap and was truncated)", err), nil
			}
			return tools.Fail("feed parse error: %v", err), nil
		}
		return tools.Ok(map[string]any{
			"status_code": resp.StatusCode,
			"feed":        feed,
		}), nil
	}

	bodyStr := string(respBody)
	if stripHTML {
		var textTruncated bool
		bodyStr, textTruncated = extractText(respBody)
		bodyTruncated = bodyTruncated || textTruncated
	}

	// Decode JSON bodies when the response advertises application/json (and we
	// were not asked to strip HTML, which implies the caller wants plain text).
	// Falls back to the raw string on parse failure so partial/invalid JSON is
	// still inspectable.
	var bodyOut any = bodyStr
	if clipped, cut := ClipResultBody(bodyStr); cut {
		bodyOut = clipped
		bodyTruncated = true
	}
	if !stripHTML && isJSONContentType(resp.Header.Get("Content-Type")) {
		var decoded any
		if err := json.Unmarshal(respBody, &decoded); err == nil && len(respBody) <= HTTPResultBodyCap {
			bodyOut = decoded
		}
	}

	data := map[string]any{
		"status_code": resp.StatusCode,
		"status":      resp.Status,
		"body":        bodyOut,
		"headers":     flattenHeaders(resp.Header),
	}
	if bodyTruncated {
		data["body_truncated"] = true
	}
	r := &tools.Result{Success: resp.StatusCode < 400, Data: data}
	if !r.Success {
		r.Error = fmt.Sprintf("the remote server answered %s; its response body is in data.body", resp.Status)
	}
	return r, nil
}

// isJSONContentType returns true when the Content-Type header advertises a
// JSON payload (application/json or a *+json suffix variant like
// application/problem+json). Parameter portion after `;` is ignored.
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	if ct == "application/json" || ct == "text/json" {
		return true
	}
	return strings.HasPrefix(ct, "application/") && strings.HasSuffix(ct, "+json")
}

// flattenHeaders converts http.Header to a simple map.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// extractText strips HTML tags and returns clean text content plus whether the
// text was clipped at the extraction cap.
func extractText(htmlBytes []byte) (string, bool) {
	tokenizer := html.NewTokenizer(strings.NewReader(string(htmlBytes)))
	var b strings.Builder
	skip := 0
	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			// Collapse whitespace and trim.
			result := strings.Join(strings.Fields(b.String()), " ")
			if len(result) > extractTextCap {
				return result[:extractTextCap], true
			}
			return result, false
		case html.StartTagToken:
			tn, _ := tokenizer.TagName()
			tag := string(tn)
			if tag == "script" || tag == "style" || tag == "noscript" {
				skip++
			}
		case html.EndTagToken:
			tn, _ := tokenizer.TagName()
			tag := string(tn)
			if tag == "script" || tag == "style" || tag == "noscript" {
				if skip > 0 {
					skip--
				}
			}
		case html.TextToken:
			if skip == 0 {
				b.WriteString(tokenizer.Token().Data)
				b.WriteByte(' ')
			}
		}
	}
}

// RSS / Atom feed parsing

type feedResult struct {
	Title       string     `json:"title"`
	Link        string     `json:"link"`
	Description string     `json:"description"`
	Items       []feedItem `json:"items"`
}

type feedItem struct {
	Title       string `json:"title"`
	Link        string `json:"link"`
	Description string `json:"description"`
	PubDate     string `json:"pub_date,omitempty"`
}

// RSS 2.0 XML structures.
type rssRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}
type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []rssItem `xml:"item"`
}
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

// Atom XML structures.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Title   string      `xml:"title"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}
type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}
type atomEntry struct {
	Title   string     `xml:"title"`
	Links   []atomLink `xml:"link"`
	Summary string     `xml:"summary"`
	Content string     `xml:"content"`
	Updated string     `xml:"updated"`
}

const maxFeedItems = 50
const maxFeedDescLen = 500

func parseRSSOrAtomFeed(data []byte) (*feedResult, error) {
	// Try RSS 2.0 first.
	var rss rssRoot
	if err := xml.Unmarshal(data, &rss); err == nil && rss.Channel.Title != "" {
		result := &feedResult{
			Title:       rss.Channel.Title,
			Link:        rss.Channel.Link,
			Description: stringutil.Truncate(rss.Channel.Description, maxFeedDescLen),
		}
		for i, item := range rss.Channel.Items {
			if i >= maxFeedItems {
				break
			}
			result.Items = append(result.Items, feedItem{
				Title:       item.Title,
				Link:        item.Link,
				Description: stringutil.Truncate(stripHTMLTags(item.Description), maxFeedDescLen),
				PubDate:     item.PubDate,
			})
		}
		return result, nil
	}

	// Try Atom.
	var atom atomFeed
	if err := xml.Unmarshal(data, &atom); err == nil && atom.Title != "" {
		link := ""
		for _, l := range atom.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		result := &feedResult{
			Title: atom.Title,
			Link:  link,
		}
		for i, entry := range atom.Entries {
			if i >= maxFeedItems {
				break
			}
			eLink := ""
			for _, l := range entry.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					eLink = l.Href
					break
				}
			}
			desc := entry.Summary
			if desc == "" {
				desc = entry.Content
			}
			result.Items = append(result.Items, feedItem{
				Title:       entry.Title,
				Link:        eLink,
				Description: stringutil.Truncate(stripHTMLTags(desc), maxFeedDescLen),
				PubDate:     entry.Updated,
			})
		}
		return result, nil
	}

	return nil, fmt.Errorf("response is not a valid RSS 2.0 or Atom feed")
}

// stripHTMLTags removes HTML markup from a string (for feed descriptions).
func stripHTMLTags(s string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			return strings.TrimSpace(b.String())
		}
		if tt == html.TextToken {
			b.WriteString(tokenizer.Token().Data)
		}
	}
}
