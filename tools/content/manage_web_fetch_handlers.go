package content

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	"openfabrica/internal/mediatype"
	"openfabrica/internal/netguard"
	"openfabrica/store"
	"openfabrica/tools"
)

const (
	webFetchMaxBodyBytes  = 4 << 20   // 4 MiB cap for text/HTML fetch
	webFetchMaxAssetBytes = 25 << 20  // 25 MiB cap for binary asset download
	webFetchMaxStyleBytes = 256 << 10 // 256 KiB cap for linked CSS inspection
	webFetchUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/158.0.0.0 Safari/537.36"
	webFetchSecCHUA       = `"Google Chrome";v="158", "Chromium";v="158", "Not_A Brand";v="24"`
	webFetchMaxRetryWait  = 5 * time.Second
)

// webFetchClientFactory, webFetchValidateURL, and webFetchDefaultRetryWait are overridden in unit tests.
var (
	webFetchClientFactory    = netguard.SSRFSafeClient
	webFetchValidateURL      = netguard.ValidateExternalURL
	webFetchDefaultRetryWait = 1 * time.Second
)

func webFetchURL(ctx *tools.ToolContext, rawURL string, offset, maxChars int, extractDesign bool) (*tools.Result, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return tools.Fail("url is required"), nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return tools.Fail("url must be a valid http or https URL"), nil
	}

	if offset < 0 {
		offset = 0
	}
	if maxChars <= 0 {
		maxChars = 8000
	}
	if maxChars < 500 {
		maxChars = 500
	}
	if maxChars > 20000 {
		maxChars = 20000
	}

	client := webFetchClientFactory()
	resp, rawBytes, err := doFetchWithRetry(ctx.Context(), client, parsed.String())
	if err != nil {
		return tools.FailErr(err), nil
	}

	bodyStr := string(rawBytes)
	if isBotInterstitial(resp.statusCode, bodyStr) {
		return tools.Fail("blocked by anti-bot challenge at %s (HTTP %d); try another URL", resp.finalURL.String(), resp.statusCode), nil
	}
	if resp.statusCode == http.StatusTooManyRequests || resp.statusCode == http.StatusServiceUnavailable {
		return tools.Fail("rate limited by %s (HTTP %d); try again later", resp.finalURL.String(), resp.statusCode), nil
	}
	if resp.statusCode < 200 || resp.statusCode >= 300 {
		return tools.Fail("HTTP %d fetching %s", resp.statusCode, resp.finalURL.String()), nil
	}

	mediaType, _, _ := mime.ParseMediaType(resp.contentType)
	if mediaType == "" {
		mediaType = http.DetectContentType(rawBytes)
		if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
			mediaType = strings.TrimSpace(mediaType[:idx])
		}
	}

	var title, extracted, description string
	var htmlDoc *html.Node
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		htmlDoc, title, extracted = extractHTMLContentDoc(bodyStr)
		if htmlDoc != nil {
			description = findMetaDescription(htmlDoc)
		}
	case mediaType == "application/rss+xml" || mediaType == "application/atom+xml" ||
		((mediaType == "application/xml" || mediaType == "text/xml") && looksLikeFeed(bodyStr)):
		title, extracted = extractFeedContent(bodyStr)
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml"):
		extracted = strings.TrimSpace(bodyStr)
	default:
		return tools.Fail("unsupported content-type %q (only text, HTML, JSON, and RSS/Atom feeds are supported)", mediaType), nil
	}

	chunk, totalChars, hasMore := sliceRunes(extracted, offset, maxChars)
	out := map[string]any{
		"url":            resp.finalURL.String(),
		"status":         resp.statusCode,
		"content_type":   mediaType,
		"content":        chunk,
		"offset":         offset,
		"returned_chars": utf8.RuneCountInString(chunk),
		"total_chars":    totalChars,
		"has_more":       hasMore,
	}
	if title != "" {
		out["title"] = title
	}
	if description != "" {
		out["description"] = description
	}
	if hasMore {
		out["next_offset"] = offset + utf8.RuneCountInString(chunk)
	}
	if htmlDoc != nil {
		if isConsentGateRedirect(parsed, resp.finalURL, title, totalChars) {
			out["consent_gate"] = true
			out["note"] = "redirected to a JS/cookie consent gate; use manage_web_search or try an RSS/subpage URL instead"
		}
		if extractDesign {
			attachHTMLDesignSignals(ctx.Context(), client, resp.finalURL, htmlDoc, out)
		}
	}
	return tools.Ok(out), nil
}

type fetchedHTTPResponse struct {
	statusCode  int
	finalURL    *url.URL
	contentType string
}

func doFetchWithRetry(ctx context.Context, client *http.Client, targetURL string) (*fetchedHTTPResponse, []byte, error) {
	return doHTTPGetWithRetry(ctx, client, targetURL, webFetchMaxBodyBytes)
}

func doHTTPGetWithRetry(ctx context.Context, client *http.Client, targetURL string, maxBytes int64) (*fetchedHTTPResponse, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("User-Agent", webFetchUserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/markdown,text/plain,application/json;q=0.8,*/*;q=0.7")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Sec-CH-UA", webFetchSecCHUA)
		req.Header.Set("Sec-CH-UA-Mobile", "?0")
		req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Upgrade-Insecure-Requests", "1")

		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("fetching url: %w", err)
		}
		rawBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("reading response: %w", readErr)
		}
		if int64(len(rawBytes)) > maxBytes {
			return nil, nil, fmt.Errorf("response exceeds %d byte limit", maxBytes)
		}

		if attempt == 0 && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) && !isBotInterstitial(resp.StatusCode, string(rawBytes)) {
			wait := parseWebFetchRetryAfter(resp.Header.Get("Retry-After"), webFetchDefaultRetryWait)
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, nil, ctx.Err()
				case <-timer.C:
				}
			}
			continue
		}
		return &fetchedHTTPResponse{
			statusCode:  resp.StatusCode,
			finalURL:    resp.Request.URL,
			contentType: resp.Header.Get("Content-Type"),
		}, rawBytes, nil
	}
	return nil, nil, fmt.Errorf("fetching url: retry exhausted")
}

func parseWebFetchRetryAfter(header string, fallback time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		if d > webFetchMaxRetryWait {
			return webFetchMaxRetryWait
		}
		return d
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		if d > webFetchMaxRetryWait {
			return webFetchMaxRetryWait
		}
		return d
	}
	return fallback
}

func isBotInterstitial(status int, body string) bool {
	if status != http.StatusOK && status != http.StatusForbidden && status != http.StatusServiceUnavailable && status != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "cf-browser-verification") ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "enable javascript and cookies to continue") ||
		strings.Contains(lower, "attention required! | cloudflare") ||
		(strings.Contains(lower, "just a moment...") && strings.Contains(lower, "cloudflare"))
}

func sliceRunes(s string, offset, maxChars int) (chunk string, total int, hasMore bool) {
	runes := []rune(s)
	total = len(runes)
	if offset >= total {
		return "", total, false
	}
	end := offset + maxChars
	if end >= total {
		return string(runes[offset:]), total, false
	}
	return string(runes[offset:end]), total, true
}

func extractHTMLContent(rawHTML string) (title, content string) {
	_, title, content = extractHTMLContentDoc(rawHTML)
	return title, content
}

func extractHTMLContentDoc(rawHTML string) (doc *html.Node, title, content string) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil, "", strings.TrimSpace(rawHTML)
	}

	title = findHTMLTitle(doc)
	body := findElement(doc, "body")
	if body == nil {
		body = doc
	}

	var sb strings.Builder
	renderHTMLNode(&sb, body, false)
	return doc, title, normalizeBlankLines(sb.String())
}

func isConsentGateRedirect(requested, final *url.URL, title string, totalChars int) bool {
	if totalChars >= 200 || final == nil {
		return false
	}
	finalStr := strings.ToLower(final.String())
	titleLower := strings.ToLower(title)
	hostChanged := requested != nil && !strings.EqualFold(requested.Hostname(), final.Hostname())
	consentURL := strings.Contains(finalStr, "consent") || strings.Contains(finalStr, "privacy-gate") || strings.Contains(finalStr, "cookie")
	consentTitle := strings.Contains(titleLower, "privacy") || strings.Contains(titleLower, "consent") || strings.Contains(titleLower, "cookie")
	return (hostChanged && (consentURL || consentTitle)) || (consentURL && consentTitle)
}

func findMetaDescription(doc *html.Node) string {
	var desc string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if desc != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "meta" {
			name := strings.ToLower(attrLookup(n, "name"))
			prop := strings.ToLower(attrLookup(n, "property"))
			if name == "description" || prop == "og:description" {
				if c := strings.TrimSpace(attrLookup(n, "content")); c != "" {
					desc = c
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return desc
}

func attrLookup(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

var (
	hexColorRe   = regexp.MustCompile(`(?i)#(?:[0-9a-f]{6}|[0-9a-f]{3})\b`)
	cssVarRe     = regexp.MustCompile(`(--[a-zA-Z0-9_-]{2,40})\s*:\s*([^;}{]{1,80})`)
	fontFamilyRe = regexp.MustCompile(`(?i)font-family\s*:\s*([^;}{]{1,120})`)
)

type linkItem struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func attachHTMLDesignSignals(ctx context.Context, client *http.Client, baseURL *url.URL, doc *html.Node, out map[string]any) {
	var navLinks []linkItem
	var feeds []linkItem
	var themeColor string
	var styleBuf strings.Builder
	var stylesheetURLs []string
	var fontNames []string
	seenNav := map[string]bool{}
	seenFeed := map[string]bool{}
	seenFont := map[string]bool{}
	addFont := func(f string) {
		f = strings.Trim(strings.TrimSpace(f), `"'`)
		if f == "" || seenFont[strings.ToLower(f)] {
			return
		}
		low := strings.ToLower(f)
		if low == "inherit" || low == "initial" || low == "unset" || low == "sans-serif" || low == "serif" || low == "monospace" || low == "system-ui" || low == "-apple-system" || low == "blinkmacsystemfont" {
			return
		}
		seenFont[low] = true
		if len(fontNames) < 8 {
			fontNames = append(fontNames, f)
		}
	}

	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inNav bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "nav", "header":
				inNav = true
			case "meta":
				if strings.EqualFold(attrLookup(n, "name"), "theme-color") && themeColor == "" {
					themeColor = strings.TrimSpace(attrLookup(n, "content"))
				}
			case "style":
				styleBuf.WriteString(collectText(n))
				styleBuf.WriteByte('\n')
			case "link":
				rel := strings.ToLower(attrLookup(n, "rel"))
				href := strings.TrimSpace(attrLookup(n, "href"))
				typ := strings.ToLower(attrLookup(n, "type"))
				if href != "" && baseURL != nil {
					if u, err := baseURL.Parse(href); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
						abs := u.String()
						if strings.Contains(rel, "alternate") && (strings.Contains(typ, "rss") || strings.Contains(typ, "atom")) {
							if !seenFeed[abs] && len(feeds) < 10 {
								seenFeed[abs] = true
								feeds = append(feeds, linkItem{Label: strings.TrimSpace(attrLookup(n, "title")), URL: abs})
							}
						} else if strings.Contains(rel, "stylesheet") {
							if strings.Contains(abs, "fonts.googleapis.com") || strings.Contains(abs, "fonts.bunny.net") {
								for _, fam := range u.Query()["family"] {
									name := strings.SplitN(fam, ":", 2)[0]
									addFont(strings.ReplaceAll(name, "+", " "))
								}
							} else if len(stylesheetURLs) < 3 {
								stylesheetURLs = append(stylesheetURLs, abs)
							}
						}
					}
				}
			case "a":
				if inNav && len(navLinks) < 20 && baseURL != nil {
					href := strings.TrimSpace(attrLookup(n, "href"))
					label := strings.Join(strings.Fields(collectText(n)), " ")
					if label != "" && href != "" && !strings.HasPrefix(href, "#") && !strings.HasPrefix(strings.ToLower(href), "javascript:") {
						if u, err := baseURL.Parse(href); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
							abs := u.String()
							if !seenNav[abs] {
								seenNav[abs] = true
								navLinks = append(navLinks, linkItem{Label: label, URL: abs})
							}
						}
					}
				}
			}
			if st := attrLookup(n, "style"); st != "" {
				styleBuf.WriteString(st)
				styleBuf.WriteByte('\n')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inNav)
		}
	}
	walk(doc, false)

	cssText := styleBuf.String()
	// If inline styles have few color tokens, inspect the first linked stylesheet.
	if len(hexColorRe.FindAllString(cssText, 4)) < 3 && len(stylesheetURLs) > 0 {
		if fetchedCSS := fetchStylesheetSnippet(ctx, client, stylesheetURLs[0]); fetchedCSS != "" {
			cssText += "\n" + fetchedCSS
		}
	}

	for _, m := range fontFamilyRe.FindAllStringSubmatch(cssText, 16) {
		for _, part := range strings.Split(m[1], ",") {
			addFont(part)
		}
	}

	colorCounts := map[string]int{}
	for _, h := range hexColorRe.FindAllString(cssText, 500) {
		colorCounts[strings.ToLower(h)]++
	}
	type colorFreq struct {
		hex   string
		count int
	}
	var cfs []colorFreq
	for h, cnt := range colorCounts {
		cfs = append(cfs, colorFreq{h, cnt})
	}
	sort.Slice(cfs, func(i, j int) bool {
		if cfs[i].count != cfs[j].count {
			return cfs[i].count > cfs[j].count
		}
		return cfs[i].hex < cfs[j].hex
	})
	var topColors []string
	for i := 0; i < len(cfs) && i < 12; i++ {
		topColors = append(topColors, cfs[i].hex)
	}

	cssVars := map[string]string{}
	for _, m := range cssVarRe.FindAllStringSubmatch(cssText, 100) {
		if len(cssVars) >= 15 {
			break
		}
		val := strings.TrimSpace(m[2])
		if val != "" && !strings.Contains(val, "url(") {
			cssVars[m[1]] = val
		}
	}

	if len(navLinks) > 0 {
		out["nav_links"] = navLinks
	}
	if len(feeds) > 0 {
		out["feeds"] = feeds
	}
	design := map[string]any{}
	if themeColor != "" {
		design["theme_color"] = themeColor
	}
	if len(fontNames) > 0 {
		design["fonts"] = fontNames
	}
	if len(topColors) > 0 {
		design["colors"] = topColors
	}
	if len(cssVars) > 0 {
		design["css_vars"] = cssVars
	}
	if len(design) > 0 {
		out["design"] = design
	}
}

func fetchStylesheetSnippet(ctx context.Context, client *http.Client, cssURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cssURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", webFetchUserAgent)
	req.Header.Set("Accept", "text/css,*/*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxStyleBytes))
	if err != nil {
		return ""
	}
	return string(b)
}

func findHTMLTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "title" {
		return strings.Join(strings.Fields(collectText(n)), " ")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := findHTMLTitle(c); t != "" {
			return t
		}
	}
	return ""
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, tag); found != nil {
			return found
		}
	}
	return nil
}

func collectText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(collectText(c))
	}
	return sb.String()
}

var skipHTMLTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true,
	"nav": true, "footer": true, "aside": true, "iframe": true,
}

func renderHTMLNode(sb *strings.Builder, n *html.Node, inPre bool) {
	if n.Type == html.TextNode {
		if inPre {
			sb.WriteString(n.Data)
		} else {
			txt := collapseInlineSpace(n.Data)
			if txt != "" {
				sb.WriteString(txt)
			}
		}
		return
	}
	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, inPre)
		}
		return
	}

	if skipHTMLTags[n.Data] {
		return
	}

	switch n.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(n.Data[1] - '0')
		ensureBlankLine(sb)
		sb.WriteString(strings.Repeat("#", level))
		sb.WriteByte(' ')
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, false)
		}
		sb.WriteString("\n\n")
	case "p", "div", "section", "article", "main", "blockquote":
		if n.Data == "blockquote" {
			ensureBlankLine(sb)
			sb.WriteString("> ")
		} else if n.Data == "p" {
			ensureBlankLine(sb)
		} else {
			ensureNewline(sb)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, inPre)
		}
		if n.Data == "p" || n.Data == "blockquote" {
			sb.WriteString("\n\n")
		} else {
			ensureNewline(sb)
		}
	case "br":
		sb.WriteByte('\n')
	case "hr":
		ensureBlankLine(sb)
		sb.WriteString("---\n\n")
	case "li":
		ensureNewline(sb)
		sb.WriteString("- ")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, false)
		}
		sb.WriteByte('\n')
	case "pre":
		ensureBlankLine(sb)
		sb.WriteString("```\n")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, true)
		}
		ensureNewline(sb)
		sb.WriteString("```\n\n")
	case "code":
		if inPre {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				renderHTMLNode(sb, c, true)
			}
			return
		}
		code := strings.TrimSpace(collectText(n))
		if code != "" {
			sb.WriteByte('`')
			sb.WriteString(code)
			sb.WriteByte('`')
		}
	case "a":
		href := ""
		for _, a := range n.Attr {
			if a.Key == "href" {
				href = strings.TrimSpace(a.Val)
				break
			}
		}
		var linkText strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(&linkText, c, false)
		}
		label := strings.TrimSpace(linkText.String())
		if label == "" {
			return
		}
		if href != "" && (strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://")) {
			fmt.Fprintf(sb, "[%s](%s)", label, href)
		} else {
			sb.WriteString(label)
		}
	case "tr":
		ensureNewline(sb)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, false)
		}
		sb.WriteByte('\n')
	case "td", "th":
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, false)
		}
		sb.WriteString(" | ")
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(sb, c, inPre)
		}
	}
}

func collapseInlineSpace(s string) string {
	if s == "" {
		return ""
	}
	hasLead := s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r'
	hasTrail := s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r'
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return " "
	}
	out := strings.Join(fields, " ")
	if hasLead {
		out = " " + out
	}
	if hasTrail {
		out += " "
	}
	return out
}

func ensureNewline(sb *strings.Builder) {
	s := sb.String()
	if len(s) > 0 && s[len(s)-1] != '\n' {
		sb.WriteByte('\n')
	}
}

func ensureBlankLine(sb *strings.Builder) {
	s := sb.String()
	if len(s) == 0 {
		return
	}
	if !strings.HasSuffix(s, "\n\n") {
		if strings.HasSuffix(s, "\n") {
			sb.WriteByte('\n')
		} else {
			sb.WriteString("\n\n")
		}
	}
}

func normalizeBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(trimmed) == "" {
			blankRun++
			if blankRun <= 1 && len(out) > 0 {
				out = append(out, "")
			}
			continue
		}
		blankRun = 0
		out = append(out, strings.TrimLeft(trimmed, " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func looksLikeFeed(s string) bool {
	head := s
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.Contains(head, "<rss") || strings.Contains(head, "<feed")
}

type rssFeed struct {
	Channel struct {
		Title string    `xml:"title"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

type atomFeed struct {
	Title   string      `xml:"title"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
	Summary string `xml:"summary"`
	Link    struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
}

func extractFeedContent(rawXML string) (title, content string) {
	var rss rssFeed
	if err := xml.Unmarshal([]byte(rawXML), &rss); err == nil && len(rss.Channel.Items) > 0 {
		var sb strings.Builder
		for _, it := range rss.Channel.Items {
			fmt.Fprintf(&sb, "- **%s**", strings.TrimSpace(it.Title))
			if it.PubDate != "" {
				fmt.Fprintf(&sb, " (%s)", strings.TrimSpace(it.PubDate))
			}
			if it.Link != "" {
				fmt.Fprintf(&sb, "\n  %s", strings.TrimSpace(it.Link))
			}
			if it.Description != "" {
				_, desc := extractHTMLContent(it.Description)
				if desc != "" {
					fmt.Fprintf(&sb, "\n  %s", desc)
				}
			}
			sb.WriteByte('\n')
		}
		return strings.TrimSpace(rss.Channel.Title), strings.TrimSpace(sb.String())
	}

	var atom atomFeed
	if err := xml.Unmarshal([]byte(rawXML), &atom); err == nil && len(atom.Entries) > 0 {
		var sb strings.Builder
		for _, e := range atom.Entries {
			fmt.Fprintf(&sb, "- **%s**", strings.TrimSpace(e.Title))
			if e.Updated != "" {
				fmt.Fprintf(&sb, " (%s)", strings.TrimSpace(e.Updated))
			}
			if e.Link.Href != "" {
				fmt.Fprintf(&sb, "\n  %s", strings.TrimSpace(e.Link.Href))
			}
			if e.Summary != "" {
				_, sum := extractHTMLContent(e.Summary)
				if sum != "" {
					fmt.Fprintf(&sb, "\n  %s", sum)
				}
			}
			sb.WriteByte('\n')
		}
		return strings.TrimSpace(atom.Title), strings.TrimSpace(sb.String())
	}

	return "", strings.TrimSpace(rawXML)
}

func sanitizeAssetFilename(raw string, extraExts []string) (string, string, error) {
	name := strings.ReplaceAll(strings.TrimSpace(raw), `\`, `/`)
	name = strings.TrimPrefix(name, "/files/")
	name = strings.TrimPrefix(name, "files/")
	name = strings.TrimPrefix(name, "/")
	if err := store.ValidateStorageFilename(name); err != nil {
		return "", "", fmt.Errorf("invalid filename %q: %w", raw, err)
	}
	ext := strings.ToLower(filepath.Ext(name))
	kind, ok := mediatype.ClassifyDownloadExt(ext, extraExts)
	if !ok {
		if mediatype.IsDangerousDownloadExt(ext) {
			return "", "", fmt.Errorf("unsupported file extension %q (executable and active web script types are blocked)", ext)
		}
		return "", "", fmt.Errorf("unsupported file extension %q (built-in: images, .glb/.gltf/.obj/.mtl, .mp3/.wav/.ogg/.m4a, .mp4/.webm, .woff/.woff2/.ttf/.otf, .pdf/.csv/.json/.txt; operator can add safe extensions via extra_download_extensions in config.json)", ext)
	}
	return name, kind, nil
}

func webDownloadAsset(ctx *tools.ToolContext, rawURL, rawFilename string) (*tools.Result, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return tools.Fail("url is required"), nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return tools.Fail("url must be a valid http or https URL"), nil
	}
	if err := webFetchValidateURL(rawURL); err != nil {
		return tools.Fail("blocked URL: %v", err), nil
	}

	cleanName, kind, err := sanitizeAssetFilename(rawFilename, ctx.ExtraDownloadExtensions)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	maxAssetBytes := int64(webFetchMaxAssetBytes)
	if ctx.WebFetchMaxAssetMB > 0 {
		maxAssetBytes = int64(ctx.WebFetchMaxAssetMB) << 20
	}
	client := webFetchClientFactory()
	resp, rawBytes, err := doHTTPGetWithRetry(ctx.Context(), client, parsed.String(), maxAssetBytes)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if isBotInterstitial(resp.statusCode, string(rawBytes)) {
		return tools.Fail("blocked by anti-bot challenge at %s (HTTP %d); try another URL", resp.finalURL.String(), resp.statusCode), nil
	}
	if resp.statusCode == http.StatusTooManyRequests || resp.statusCode == http.StatusServiceUnavailable {
		return tools.Fail("rate limited by %s (HTTP %d); try again later", resp.finalURL.String(), resp.statusCode), nil
	}
	if resp.statusCode < 200 || resp.statusCode >= 300 {
		return tools.Fail("HTTP %d downloading %s", resp.statusCode, resp.finalURL.String()), nil
	}
	if len(rawBytes) == 0 {
		return tools.Fail("downloaded file is empty"), nil
	}

	mediaType, _, _ := mime.ParseMediaType(resp.contentType)
	if mediaType == "" {
		mediaType = http.DetectContentType(rawBytes)
		if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
			mediaType = strings.TrimSpace(mediaType[:idx])
		}
	}
	if !mediatype.IsAllowedDownloadContentType(kind, mediaType) {
		return tools.Fail("content-type %q does not match %s asset %q", mediaType, kind, cleanName), nil
	}

	finalURL := resp.finalURL.String()
	if _, err := SaveBytesAsArtifact(ctx, rawBytes, mediaType, cleanName, map[string]any{"source_url": finalURL}); err != nil {
		return tools.FailErr(err), nil
	}

	return tools.Ok(map[string]any{
		"path":         "/files/" + cleanName,
		"filename":     cleanName,
		"kind":         kind,
		"content_type": mediaType,
		"bytes":        len(rawBytes),
		"source_url":   finalURL,
	}), nil
}
