package store

import (
	"fmt"
	"regexp"
	"strings"

	"openfabrica/analysis"
)

// validateNoCrossOriginStylesheets rejects <link rel="stylesheet"> tags whose
// href points to a different origin: a malformed URL returns HTML that Chrome's
// ORB blocks on first paint, and they require a brittle integrity= hash. The
// strict equivalent is @import url(...) inside a global-scope CSS artifact,
// served same-origin so SRI/ORB/CORS never apply. Collects every offending href
// so the model can fix the whole class in one save.
//
// AssetLoadingNote is the ONE canonical statement of how JS/CSS assets reach
// the page. Every asset-tag rejection ends with it, so a model bounced between
// the cross-origin and same-origin gates always sees the SAME complete channel
// map; case-specific fast fixes still lead each message.
const AssetLoadingNote = "Canonical asset channels (never hand-write `<script>`/`<link>` tags in a page or layout body): page-scoped JS/CSS - save it as kind=file scope=page and attach it (manage_files action=attach, or attach_to on the save); sitewide JS/CSS - save it as kind=file scope=global, auto-injected via {{assets}}; third-party/CDN assets - declare via manage_libraries action=add_library (or plan.external_libraries), auto-injected via {{assets}}."

func validateNoCrossOriginStylesheets(body, kind string) error {
	var hrefs []string
	for _, m := range analysis.LinkTagRe.FindAllStringSubmatch(body, -1) {
		attrs := m[1]
		if strings.ToLower(analysis.ExtractAttr(attrs, "rel")) != "stylesheet" {
			continue
		}
		href := analysis.ExtractAttr(attrs, "href")
		if !analysis.CrossOriginURL(href) {
			continue
		}
		hrefs = append(hrefs, href)
	}
	if len(hrefs) == 0 {
		return nil
	}
	var b strings.Builder
	if len(hrefs) == 1 {
		fmt.Fprintf(&b, "%s contains <link rel=\"stylesheet\" href=%q>. ", kind, hrefs[0])
	} else {
		fmt.Fprintf(&b, "%s contains %d cross-origin <link rel=\"stylesheet\"> tags:\n", kind, len(hrefs))
		for _, h := range hrefs {
			fmt.Fprintf(&b, "  - %s\n", h)
		}
	}
	b.WriteString("Cross-origin stylesheet links break in two ways: a malformed URL returns HTML and Chrome's ORB blocks the page on first paint, and they require an integrity= hash that's brittle to maintain. ")
	b.WriteString("For fonts and other CDN stylesheets, call `manage_libraries action=add_library` with `library={name:\"<lib>\", url:\"" + hrefs[0] + "\"}` ")
	if len(hrefs) > 1 {
		b.WriteString("(repeat for each URL above) ")
	}
	b.WriteString("then retry this save with the <link> tag(s) removed - the platform injects the URL into <head> through {{assets}} automatically. ")
	b.WriteString("As a fallback for non-plan cases, move the import into a global-scope CSS artifact:\n")
	b.WriteString("  manage_files save kind=file scope=global file_path=\"css/fonts.css\" body=`")
	for i, h := range hrefs {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "@import url(%q);", h)
	}
	b.WriteString("`\nThen remove the <link> tag(s), global CSS auto-injects via {{assets}}")
	return fmt.Errorf("%s", b.String())
}

// validateNoCrossOriginScripts rejects raw <script src="https://..."> tags in
// layouts and pages: cross-origin scripts must be declared in
// plan.external_libraries so the platform injects the tag with
// crossorigin="anonymous" and any integrity= hash - a compromised CDN can't
// silently swap bytes on a hand-written tag nobody owns. <script> blocks with
// no src= are allowed; only the network-fetching cross-origin form is gated.
func validateNoCrossOriginScripts(body, kind string) error {
	var srcs []string
	for _, m := range analysis.ScriptTagRe.FindAllStringSubmatch(body, -1) {
		attrs := m[1]
		src := analysis.ExtractAttr(attrs, "src")
		if src == "" || !analysis.CrossOriginURL(src) {
			continue
		}
		srcs = append(srcs, src)
	}
	if len(srcs) == 0 {
		return nil
	}
	var b strings.Builder
	if len(srcs) == 1 {
		fmt.Fprintf(&b, "%s contains <script src=%q>. ", kind, srcs[0])
	} else {
		fmt.Fprintf(&b, "%s contains %d cross-origin <script src> tags:\n", kind, len(srcs))
		for _, s := range srcs {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	}
	b.WriteString("Cross-origin scripts must be declared in plan.external_libraries (the platform emits the <script> tag with crossorigin=\"anonymous\" and integrity= when supplied), not hand-written in the layout. To recover: ")
	b.WriteString("(1) call `manage_libraries action=add_library` with `library={name:\"<lib>\", url:\"")
	b.WriteString(srcs[0])
	b.WriteString("\"}` ")
	if len(srcs) > 1 {
		b.WriteString("(repeat for each URL above) ")
	}
	b.WriteString("- add `kind:\"module\"` + `module_name:\"<bare specifier>\"` if it is an ES-module build (.mjs, /esm/, /es/); ")
	b.WriteString("(2) retry this save with the <script> tag(s) removed - the auto-injector will fill them back into <head> on the next layout write.")
	return fmt.Errorf("%s", b.String())
}

// validateNoSameOriginAssetTags rejects hand-written <script src="..."> and
// <link rel="stylesheet" href="..."> tags in page/layout bodies whose URL is
// same-origin (starts with `/` or has no scheme). The canonical channels are
// attach (page scope) and {{assets}} auto-injection (global scope), so a
// hand-rolled tag either races/duplicates the auto-injection or dereferences a
// path nothing serves - a dead tag that 404s on every load. Hard-reject with a
// redirect to attach; validateNoCrossOrigin{Scripts,Stylesheets} above cover
// the cross-origin equivalent.
func validateNoSameOriginAssetTags(body, kind string) error {
	type bad struct {
		tag  string // "script" or "link"
		attr string // src or href value
	}
	var offenses []bad
	for _, m := range analysis.ScriptTagRe.FindAllStringSubmatch(body, -1) {
		src := analysis.ExtractAttr(m[1], "src")
		if src == "" || analysis.CrossOriginURL(src) {
			continue
		}
		offenses = append(offenses, bad{tag: "script", attr: src})
	}
	for _, m := range analysis.LinkTagRe.FindAllStringSubmatch(body, -1) {
		if strings.ToLower(analysis.ExtractAttr(m[1], "rel")) != "stylesheet" {
			continue
		}
		href := analysis.ExtractAttr(m[1], "href")
		if href == "" || analysis.CrossOriginURL(href) {
			continue
		}
		offenses = append(offenses, bad{tag: "link", attr: href})
	}
	if len(offenses) == 0 {
		return nil
	}
	var b strings.Builder
	if len(offenses) == 1 {
		o := offenses[0]
		if o.tag == "script" {
			fmt.Fprintf(&b, "%s contains <script src=%q>. ", kind, o.attr)
		} else {
			fmt.Fprintf(&b, "%s contains <link rel=\"stylesheet\" href=%q>. ", kind, o.attr)
		}
	} else {
		fmt.Fprintf(&b, "%s contains %d hand-written same-origin asset tag(s):\n", kind, len(offenses))
		for _, o := range offenses {
			if o.tag == "script" {
				fmt.Fprintf(&b, "  - <script src=%q>\n", o.attr)
			} else {
				fmt.Fprintf(&b, "  - <link rel=\"stylesheet\" href=%q>\n", o.attr)
			}
		}
	}
	b.WriteString("Remove the hand-written tag(s) and use the canonical channel. ")
	b.WriteString(AssetLoadingNote)
	return CodedErrorf("hand_written_asset_tag", "%s", b.String())
}

// validateNoCrossOriginAssets runs the stylesheet and script checks and joins
// both errors so one save surfaces every external-tag fix the model needs to
// make. Returning them serially (one save per offense) forces the model to
// rebuild and re-send the full body for every class of fix.
func validateNoCrossOriginAssets(body, kind string) error {
	var parts []string
	if err := validateNoCrossOriginStylesheets(body, kind); err != nil {
		parts = append(parts, err.Error())
	}
	if err := validateNoCrossOriginScripts(body, kind); err != nil {
		parts = append(parts, err.Error())
	}
	if len(parts) == 0 {
		return nil
	}
	// Close with the shared channel map so this surface and the same-origin gate
	// (validateNoSameOriginAssetTags) leave the model the identical canonical
	// guidance, whichever asset gate it tripped.
	parts = append(parts, AssetLoadingNote)
	return fmt.Errorf("%s", strings.Join(parts, "\n\n"))
}

// validateNoCrossOriginScriptInjection rejects a JS artifact (kind=file) that
// pulls in a cross-origin script at runtime - injected via
// document.createElement('script') with a cross-origin .src, or imported from an
// absolute CDN URL. The HTML gate validateNoCrossOriginScripts only sees
// <script src> tags in page/layout bodies, so this covers the JS channel a weak
// model reaches for when it wants a library: the hand-injected/absolute-URL form
// bypasses registration (the renderer can't emit crossorigin="anonymous"/integrity=,
// and a hardened CSP won't run the unregistered origin), and it fails only at
// runtime with no hint to declare the library. Conservative - see
// analysis.CrossOriginScriptInjection for exactly what qualifies.
func validateNoCrossOriginScriptInjection(body, kind string) error {
	urls := analysis.CrossOriginScriptInjection(body)
	if len(urls) == 0 {
		return nil
	}
	var b strings.Builder
	if len(urls) == 1 {
		fmt.Fprintf(&b, "%s loads a cross-origin script at runtime: %s. ", kind, urls[0])
	} else {
		fmt.Fprintf(&b, "%s loads %d cross-origin scripts at runtime:\n", kind, len(urls))
		for _, u := range urls {
			fmt.Fprintf(&b, "  - %s\n", u)
		}
	}
	b.WriteString("A CDN script pulled in with createElement('script')+.src or an absolute-URL import bypasses library registration - the platform can't emit crossorigin=\"anonymous\"/integrity=, and under a hardened CSP the unregistered origin won't run. To recover: ")
	b.WriteString("(1) call `manage_libraries action=add_library` with `library={name:\"<lib>\", url:\"")
	b.WriteString(urls[0])
	b.WriteString("\"}` ")
	if len(urls) > 1 {
		b.WriteString("(repeat for each URL above) ")
	}
	b.WriteString("- add `kind:\"module\"` + `module_name:\"<bare specifier>\"` if it is an ES-module build (.mjs, /esm/, /es/); ")
	b.WriteString("(2) remove the createElement/import and reference the library's window global (classic) or import it by its bare specifier (module) - the platform auto-injects the tag. ")
	b.WriteString(AssetLoadingNote)
	return fmt.Errorf("%s", b.String())
}

// sandboxAttrRe captures the value of a sandbox attribute set as an HTML
// attribute (`sandbox="allow-scripts"`) or a JS property assignment
// (`iframe.sandbox = 'allow-scripts'`). Both forms write `sandbox=<quoted>`.
var sandboxAttrRe = regexp.MustCompile(`(?is)sandbox\s*=\s*["']([^"']*)["']`)

// sandboxSetAttrRe captures the value of a sandbox set via
// `setAttribute('sandbox', 'allow-scripts')` (value is a separate string arg,
// so the `sandbox=` form above never matches it).
var sandboxSetAttrRe = regexp.MustCompile(`(?is)setAttribute\(\s*["']sandbox["']\s*,\s*["']([^"']*)["']\s*\)`)

// iframeDocAccessRe matches a parent reaching into a frame's document:
// `.contentDocument` or `.contentWindow.document`.
var iframeDocAccessRe = regexp.MustCompile(`(?is)\.contentDocument\b|\.contentWindow\s*\.\s*document\b`)

// sandboxValueIsBlocking reports whether a sandbox token list enables scripts
// but withholds same-origin, the exact combination that forces the frame into
// a unique opaque origin. A parent that then touches contentDocument hits a
// cross-origin SecurityError.
func sandboxValueIsBlocking(value string) bool {
	tokens := strings.Fields(strings.ToLower(value))
	hasScripts, hasSameOrigin := false, false
	for _, t := range tokens {
		switch t {
		case "allow-scripts":
			hasScripts = true
		case "allow-same-origin":
			hasSameOrigin = true
		}
	}
	return hasScripts && !hasSameOrigin
}

// validateNoBlockedIframeDocAccess rejects a body that builds a sandboxed
// iframe with `allow-scripts` but no `allow-same-origin`, then writes into its
// document via `contentDocument` / `contentWindow.document`: that combination
// forces the frame into a unique opaque origin, so the access throws a
// cross-origin SecurityError at runtime - interaction-gated, where the runtime
// probe routinely misses it. Both signals must co-occur in this one body
// (srcdoc-only previews and allow-same-origin frames pass); the two signals
// split across separate bodies is a known gap.
func validateNoBlockedIframeDocAccess(body, kind string) error {
	if !iframeDocAccessRe.MatchString(body) {
		return nil
	}
	blocking := false
	for _, m := range sandboxAttrRe.FindAllStringSubmatch(body, -1) {
		if sandboxValueIsBlocking(m[1]) {
			blocking = true
			break
		}
	}
	if !blocking {
		for _, m := range sandboxSetAttrRe.FindAllStringSubmatch(body, -1) {
			if sandboxValueIsBlocking(m[1]) {
				blocking = true
				break
			}
		}
	}
	if !blocking {
		return nil
	}
	return fmt.Errorf(
		"%s builds an iframe with sandbox=\"allow-scripts\" (no allow-same-origin) and then reads its document "+
			"via contentDocument/contentWindow.document. That sandbox forces the frame into a unique opaque origin, "+
			"so the parent's document access throws \"Blocked a frame ... from accessing a cross-origin frame\" at runtime. "+
			"Fix one of two ways: (1) keep it sandboxed and set the HTML with iframe.srcdoc = html instead of writing to "+
			"contentDocument (scripts inside still run); or (2) if you must script into the frame from the parent, add "+
			"allow-same-origin to the sandbox token list (only safe for first-party content you generate).",
		kind,
	)
}
