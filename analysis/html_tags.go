package analysis

// Minimal HTML-tag pattern matchers shared between artifact save-time validation
// and post-build holistic validators. Intentionally regex-based: bodies are small
// and a real HTML parser would be a heavy dependency for a save-time check.

import (
	"regexp"
	"strings"
)

// ScriptTagRe matches a full <script ...> opening tag (so we can scan its
// attributes), capturing the tag's body for inspection.
var ScriptTagRe = regexp.MustCompile(`(?is)<script\b([^>]*)>`)

// LinkTagRe matches a full <link ...> tag, capturing its body. <link>s are
// self-closing in practice, match either with or without a closing slash.
var LinkTagRe = regexp.MustCompile(`(?is)<link\b([^>]*)/?>`)

// IntegrityAttrRe matches the integrity="..." attribute on a tag so callers
// can gate the SRI warning per-tag.
var IntegrityAttrRe = regexp.MustCompile(`(?i)\bintegrity\s*=\s*["'][^"']+["']`)

// CrossOriginURL returns true when the URL targets an off-host CDN (anything
// with an explicit https:// or http:// scheme to a non-localhost host).
// Same-origin paths ("/files/x.js", "x.js") return false.
func CrossOriginURL(rawURL string) bool {
	u := strings.TrimSpace(strings.ToLower(rawURL))
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") &&
		!strings.HasPrefix(u, "//") {
		return false
	}
	hostPart := u
	for _, p := range []string{"http://", "https://", "//"} {
		hostPart = strings.TrimPrefix(hostPart, p)
	}
	if i := strings.IndexAny(hostPart, "/?#"); i >= 0 {
		hostPart = hostPart[:i]
	}
	if hostPart == "" {
		return false
	}
	if hostPart == "localhost" || strings.HasPrefix(hostPart, "127.") ||
		hostPart == "0.0.0.0" || strings.HasSuffix(hostPart, ".localhost") {
		return false
	}
	return true
}

// ExternalURLIsCSS reports whether a declared external-library URL should load as
// a stylesheet rather than a script. Single source of truth shared by the
// build-time validator and the render-time injector so they cannot disagree.
//
// Rules (precedence top-down):
//  1. Path ends in ".css"                              -> CSS
//  2. Host is fonts.googleapis.com or fonts.bunny.net  -> CSS
//  3. Path/query contains "css2?" or "/css?"           -> CSS
//  4. Otherwise                                        -> not CSS (JS)
func ExternalURLIsCSS(rawURL string) bool {
	u := strings.ToLower(rawURL)
	pathOnly := u
	if i := strings.Index(pathOnly, "?"); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	if strings.HasSuffix(pathOnly, ".css") {
		return true
	}
	if strings.Contains(u, "fonts.googleapis.com") || strings.Contains(u, "fonts.bunny.net") {
		return true
	}
	return strings.Contains(u, "css2?") || strings.Contains(u, "/css?")
}

// ExternalURLIsDynamicCSS reports whether a stylesheet URL is a font-provider
// endpoint whose bytes vary per client (User-Agent negotiation), so a build-time
// SRI hash can never match; callers skip SRI backfill and integrity= emission.
// Versioned .css files on an ordinary CDN are NOT dynamic and keep SRI.
func ExternalURLIsDynamicCSS(rawURL string) bool {
	u := strings.ToLower(rawURL)
	if strings.Contains(u, "fonts.googleapis.com") || strings.Contains(u, "fonts.bunny.net") {
		return true
	}
	return strings.Contains(u, "css2?") || strings.Contains(u, "/css?")
}

// ExternalURLIsESM reports whether a URL points to an ES-module build of a JS
// library, so callers emit type="module" (a classic <script src> aborts with
// "Unexpected token 'export'"). Path-shape based; never fetches the file.
//
// Heuristics (any one is enough):
//   - path ends in ".mjs" or ".module.js"
//   - path segment contains /es/, /esm/, /dist/es/, /dist/esm/, or /jsm/
//   - query string contains "module" (e.g. ?module, ?type=module)
func ExternalURLIsESM(rawURL string) bool {
	u := strings.ToLower(rawURL)
	pathOnly := u
	query := ""
	if i := strings.Index(u, "?"); i >= 0 {
		pathOnly = u[:i]
		query = u[i+1:]
	}
	if strings.HasSuffix(pathOnly, ".mjs") || strings.HasSuffix(pathOnly, ".module.js") {
		return true
	}
	if strings.Contains(pathOnly, "/esm/") || strings.Contains(pathOnly, "/dist/esm/") {
		return true
	}
	if strings.Contains(pathOnly, "/es/") || strings.Contains(pathOnly, "/dist/es/") {
		return true
	}
	if strings.Contains(pathOnly, "/jsm/") {
		return true
	}
	if query != "" && strings.Contains(query, "module") {
		return true
	}
	return false
}

// fontFileExts are the binary font-file suffixes a raw URL can carry. A URL
// ending in one of these (or served from fonts.gstatic.com) is a font FILE, not
// a loadable stylesheet. See ExternalURLIsFontFile.
var fontFileExts = []string{".woff2", ".woff", ".ttf", ".otf", ".eot"}

// ExternalURLIsFontFile reports whether a declared external-library URL points at
// a raw font BINARY rather than a loadable stylesheet: the path ends in a known
// font suffix (.woff2/.woff/.ttf/.otf/.eot), or the host is fonts.gstatic.com.
// The add_library gate rejects such URLs; the injector preloads them as defense-in-depth.
func ExternalURLIsFontFile(rawURL string) bool {
	u := strings.ToLower(strings.TrimSpace(rawURL))
	if u == "" {
		return false
	}
	pathOnly := u
	if i := strings.Index(pathOnly, "?"); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	for _, ext := range fontFileExts {
		if strings.HasSuffix(pathOnly, ext) {
			return true
		}
	}
	return strings.Contains(u, "fonts.gstatic.com")
}

// FontFileMIMEType returns the preload type= value for a font-file URL
// (font/woff2, font/woff, ...), or "" when the suffix is unknown. The renderer
// stamps it onto the <link rel="preload" as="font"> it emits so the browser can
// skip a format it cannot use.
func FontFileMIMEType(rawURL string) string {
	pathOnly := strings.ToLower(strings.TrimSpace(rawURL))
	if i := strings.Index(pathOnly, "?"); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	switch {
	case strings.HasSuffix(pathOnly, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(pathOnly, ".woff"):
		return "font/woff"
	case strings.HasSuffix(pathOnly, ".ttf"):
		return "font/ttf"
	case strings.HasSuffix(pathOnly, ".otf"):
		return "font/otf"
	case strings.HasSuffix(pathOnly, ".eot"):
		return "application/vnd.ms-fontobject"
	default:
		return ""
	}
}

// ExtractAttr returns the value of attrName from an HTML attribute blob like
// ` src="x.js" defer integrity="..."`. Empty string if absent. Case-insensitive
// attribute name match.
func ExtractAttr(attrs, attrName string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(attrName) + `\s*=\s*["']([^"']*)["']`)
	m := re.FindStringSubmatch(attrs)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
