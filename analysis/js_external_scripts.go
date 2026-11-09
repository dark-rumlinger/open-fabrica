package analysis

// Cross-origin script loading from JS artifacts. The HTML tag matchers in
// html_tags.go catch a hand-written <script src="cdn"> in a page/layout body;
// this file catches the equivalent reached for from JS (kind=file): a script
// injected via document.createElement('script') with a cross-origin .src, or a
// module imported from an absolute CDN URL. Both bypass library registration, so
// the renderer never learns the dependency (no crossorigin/integrity) and a
// hardened CSP won't run the unregistered origin. Regex-based to match the rest
// of this package; bodies are small and never fetched.

import (
	"regexp"
	"strings"
)

var (
	// createElementScriptRe detects a <script> element being created in JS.
	createElementScriptRe = regexp.MustCompile(`(?is)createElement\(\s*["']script["']\s*\)`)
	// srcAssignRe captures a URL assigned to a .src property (el.src = "…").
	srcAssignRe = regexp.MustCompile(`(?is)\.src\s*=\s*["']([^"']+)["']`)
	// setAttributeSrcRe captures a URL set via setAttribute('src', "…").
	setAttributeSrcRe = regexp.MustCompile(`(?is)setAttribute\(\s*["']src["']\s*,\s*["']([^"']+)["']\s*\)`)
	// importFromURLRe captures the specifier of `import … from "<url>"`. The
	// [^;'"] guard keeps the match inside one statement so it can't skip a quote.
	importFromURLRe = regexp.MustCompile(`(?is)\bimport\b[^;'"]*?\bfrom\s*["']([^"']+)["']`)
	// importSideEffectRe captures a bare side-effect import: `import "<url>"`.
	importSideEffectRe = regexp.MustCompile(`(?is)\bimport\s*["']([^"']+)["']`)
	// dynamicImportRe captures a dynamic `import("<url>")` specifier.
	dynamicImportRe = regexp.MustCompile(`(?is)\bimport\(\s*["']([^"']+)["']\s*\)`)
	// imageURLExtRe matches a URL whose path is an image file, so a cross-origin
	// image .src (legitimate) is not mistaken for a script .src.
	imageURLExtRe = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|svg|webp|avif|ico|bmp)(\?|#|$)`)
)

// CrossOriginScriptInjection reports the distinct cross-origin URLs a JS body
// loads as executable script, so the caller can steer the model to declare them
// via manage_libraries instead. Two signals, both conservative:
//
//   - an absolute cross-origin URL used as an import specifier (static
//     `import … from`, side-effect `import "…"`, or dynamic `import("…")`) - an
//     absolute-URL import always bypasses the importmap registration; and
//   - a cross-origin, non-image/non-font URL assigned to a .src (or via
//     setAttribute('src', …)) in a body that ALSO creates a <script> element -
//     the createElement('script') co-occurrence is what distinguishes a script
//     injection from a legitimate cross-origin <img>/<iframe> src.
//
// Bare specifiers, relative paths, same-origin/localhost URLs, and image/font
// files are never reported. Returns nil when nothing qualifies.
func CrossOriginScriptInjection(body string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] || !CrossOriginURL(u) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	// Absolute-URL imports are unambiguously module/script loads.
	for _, re := range []*regexp.Regexp{importFromURLRe, importSideEffectRe, dynamicImportRe} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			add(m[1])
		}
	}
	// Dynamic <script> injection: only when a <script> is being created, and only
	// for script-like (non-image, non-font) cross-origin URLs.
	if createElementScriptRe.MatchString(body) {
		for _, re := range []*regexp.Regexp{srcAssignRe, setAttributeSrcRe} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				u := m[1]
				if CrossOriginURL(u) && !imageURLExtRe.MatchString(u) && !ExternalURLIsFontFile(u) {
					add(u)
				}
			}
		}
	}
	return out
}
