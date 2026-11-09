package store

import (
	"fmt"
	"regexp"
	"strings"
)

// pageShellTokenRe matches a document-level HTML tag (<!doctype>, <html>, <body>)
// in a would-be page body. Case-insensitive; the capture group is the tag name.
var pageShellTokenRe = regexp.MustCompile(`(?i)<(!doctype|html|body)\b`)

// duplicateAuthMeFetchRe matches a fetch() call whose URL string ends in `/me`
// (with or without `/api/<path>/` prefix). This is the smoking-gun pattern of
// a page reimplementing the auth bootstrap.
var duplicateAuthMeFetchRe = regexp.MustCompile(`fetch\s*\(\s*['"\x60][^'"\x60]*/me['"\x60]`)

// duplicateAuthGuardRe matches a duplicated guard-helper definition.
var duplicateAuthGuardRe = regexp.MustCompile(`function\s+(?:requireAdmin|requireRole|requireAuth)\s*\(`)

// authStateRefRe matches a read of the bootstrap's auth state.
var authStateRefRe = regexp.MustCompile(`window\.__(?:user|authReady)\b`)

// apiFetchCallRe matches a call to the platform apiFetch helper (window-qualified
// or bare). apiFetch resolves to the raw fetch Response, not parsed JSON.
var apiFetchCallRe = regexp.MustCompile(`\bapiFetch\s*\(`)

// envelopeFieldReadRe matches a read of a list-envelope field (.rows / .count, or
// the .data alias). On the parsed JSON body these are correct; read
// straight off an unparsed Response they are always undefined.
var envelopeFieldReadRe = regexp.MustCompile(`\.(?:rows|data|count)\b`)

// bodyParseRe matches evidence that the response body was actually parsed before
// those fields were read: .json()/.text() on the Response, or JSON.parse. Its
// total absence alongside an apiFetch call and a .data/.count read is the
// signature of the "consumed the Response as if it were JSON" bug.
var bodyParseRe = regexp.MustCompile(`\.(?:json|text)\s*\(|JSON\.parse\s*\(`)

// authBootstrapPath is the home of the project's auth bootstrap (platform-emitted
// and platform_managed; see tools/auth/auth_helper.go). It is now an ES module
// (specifier "auth"). The body checks below skip this file - it IS the bootstrap -
// and steer every OTHER artifact toward importing its helpers by specifier.
const authBootstrapPath = "js/auth.js"

// validateAuthBootstrapUsage rejects page/layout/JS bodies that re-implement the
// browser auth bootstrap instead of importing it: a direct /me fetch, a
// hand-defined guard, or a synchronous window.__user/__authReady read. All three
// race the async bootstrap and manifest as the /admin <-> /login redirect loop.
// The bootstrap file itself (js/auth.js) is exempt - it IS the bootstrap.
func validateAuthBootstrapUsage(a *Artifact) error {
	if a.Kind == "file" && a.Path == authBootstrapPath {
		return nil // this IS the bootstrap
	}
	// Only check artifacts the browser executes as JS: pages (inline
	// <script>), layouts (same), and .js files.
	body := a.Body
	if body == "" {
		return nil
	}
	switch a.Kind {
	case "page", "layout":
		// fall through
	case "file":
		if !strings.HasSuffix(strings.ToLower(a.Path), ".js") {
			return nil
		}
	default:
		return nil
	}
	if duplicateAuthMeFetchRe.MatchString(body) {
		return fmt.Errorf(
			"page JS calls /me directly. The auth bootstrap `%s` already fetches /me once on load and "+
				"exports `user` / `requireRole(role)` / `requireAuth()` (import them by specifier 'auth'). "+
				"Replace the fetch with:\n"+
				"  import { requireRole } from 'auth';\n"+
				"  requireRole('admin').then(function (user) { /* ... */ });\n"+
				"Duplicate /me calls race the shared bootstrap and produce the /admin <-> /login flash loop.",
			authBootstrapPath,
		)
	}
	if duplicateAuthGuardRe.MatchString(body) {
		return fmt.Errorf(
			"this artifact defines its own auth guard (requireAdmin / requireRole / requireAuth). "+
				"The auth bootstrap `%s` exports `requireRole(role)` and `requireAuth()` - import and call "+
				"those instead:\n"+
				"  import { requireRole, requireAuth } from 'auth';\n"+
				"A hand-rolled page-side guard races the async shared bootstrap and produces redirect loops.",
			authBootstrapPath,
		)
	}
	// window.__user / window.__authReady no longer exist (auth is an ES module
	// imported by specifier 'auth'). ANY read of them is a dead synchronous access
	// that always sees "logged out" and bounces a valid session - reject it.
	if authStateRefRe.MatchString(body) {
		return fmt.Errorf(
			"page JS reads the legacy window.__user / window.__authReady globals, which no longer exist - "+
				"`%s` is an ES module (specifier 'auth') that EXPORTS `authReady` (a Promise<user|null>) and a "+
				"live-binding `user` (null until it resolves). A synchronous read always sees \"logged out\" and "+
				"bounces a valid session back to /login. Import and await instead:\n"+
				"  import { requireAuth, redirectIfLoggedIn, authReady } from 'auth';\n"+
				"  requireAuth().then(function (user) { /* render for user, or null */ });\n"+
				"  // or on /login: redirectIfLoggedIn('/');\n"+
				"  // or directly: authReady.then(function (user) { ... });",
			authBootstrapPath,
		)
	}
	// apiFetch resolves to the raw Response, not parsed JSON: reading a list-
	// envelope field without ever parsing the body yields undefined and the list
	// silently renders empty. Author-time backstop for the class the runtime
	// probe's Response tripwire catches dynamically.
	if apiFetchCallRe.MatchString(body) && envelopeFieldReadRe.MatchString(body) && !bodyParseRe.MatchString(body) {
		return fmt.Errorf(
			"page JS reads a list-envelope field (.rows / .count) off an `apiFetch(...)` result but never "+
				"calls `.json()`. `apiFetch` (imported from the auth bootstrap `%s` by specifier 'auth') resolves "+
				"to the raw fetch Response - it only adds the Bearer header and redirects on 401, it does NOT parse "+
				"the body. Reading `.rows`/`.count` straight off it yields `undefined`, so the list renders empty and "+
				"an `if (!rows.length) throw` guard flips the page into its error state. Parse first, then read "+
				"`.rows` (the list array; never a second `.data`):\n"+
				"  import { apiFetch } from 'auth';\n"+
				"  const { rows } = await (await apiFetch('users')).json();   // LIST envelope\n"+
				"  const row      = await (await apiFetch('users/' + id)).json(); // bare row",
			authBootstrapPath,
		)
	}
	return nil
}

// validatePageBody rejects page bodies that carry a full-document shell or a
// layout-only placeholder. Pages are fragments: the layout wraps every page with
// the document scaffolding and owns {{content}}/{{assets}}, so a page carrying
// either is layout content misfiled as a page - it renders the token literally
// (render.go substitutes {{content}} in the layout only, once) or double-injects
// the asset block. This is the page-side mirror of validateLayoutBody.
func validatePageBody(body string) error {
	// Coded (page_not_fragment / page_has_layout_token) so triage classifies these
	// as healthy pre-ship catches - the gate caught a page/layout scramble the model
	// then self-corrects - rather than build friction.
	if m := pageShellTokenRe.FindStringSubmatch(body); m != nil {
		// A <body>/<html> tag in a page is usually an attempt to style the whole page
		// (a dark full-screen view). There is no per-page body attribute, and a JS-added
		// body class lands after first paint, so say what does work.
		styling := ""
		if tag := strings.ToLower(m[1]); tag == "body" || tag == "html" {
			styling = ". A page cannot set attributes on <body> or <html>: to style the " +
				"whole page (a background, a dark theme), put a class on this fragment's " +
				"root element and style that (e.g. min-height:100vh), or target it from CSS " +
				"with body:has(.your-root). Never add a body class from JS - scripts run " +
				"after first paint, so the page flashes unstyled first"
		}
		return CodedErrorf("page_not_fragment",
			"page body must be an HTML fragment, found <%s>. The layout provides "+
				"<!DOCTYPE>, <html>, <head>, and <body>. Save only the page-specific "+
				"content that goes inside <body> (e.g. <section>...</section>)%s", m[1], styling,
		)
	}
	// {{content}}/{{assets}} are layout scaffolding, never page content; their
	// presence means a layout body was saved as a page (the "wrong slots"
	// scramble). {{include:name}} and any other token are left to page templating.
	for _, m := range layoutTokenRe.FindAllStringSubmatch(body, -1) {
		if token := m[1]; token == "content" || token == "assets" {
			return CodedErrorf("page_has_layout_token",
				"page body contains the layout placeholder {{%s}}, which is layout "+
					"scaffolding, not page content. Save this as kind=layout, or remove "+
					"the placeholder; a page is a fragment that fills the layout's {{content}}",
				token,
			)
		}
	}
	return nil
}

// reservedPagePaths lists anti-patterns for page paths that would silently
// 404 under the public router. The map value is the canonical alternative.
var reservedPagePaths = map[string]string{
	"/index":      "/",
	"/index.html": "/",
	"/home":       "/",
	"/home.html":  "/",
}

// ValidateFilePath enforces invariants that apply to every artifact kind before
// any kind-specific check runs. Defense-in-depth: DiskPath is always server-
// generated, so the LLM-supplied `path` never reaches the filesystem directly,
// but rejecting traversal segments / control chars here keeps the surface narrow.
func ValidateFilePath(kind, path string) error {
	if path == "" {
		return fmt.Errorf("path is required for %s", kind)
	}
	if len(path) > 256 {
		return fmt.Errorf("path is %d chars (max 256), use a shorter name", len(path))
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path contains a control character (0x%02x), use only printable characters", r)
		}
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q contains a `..` segment, paths cannot traverse upward", path)
		}
	}
	switch kind {
	case "layout", "component":
		// Layouts and components are referenced by name, never by URL. A `/`
		// would imply nesting that the renderer doesn't honour, so reject early.
		// When trimming slashes leaves a bare single segment (e.g. "/default"),
		// hand back the corrected name so the retry is one step - matching the
		// reject-with-suggestion idiom in validatePagePath.
		if strings.Contains(path, "/") {
			if bare := strings.Trim(path, "/"); bare != "" && !strings.Contains(bare, "/") {
				return fmt.Errorf("%s path %q is referenced by bare name, not URL - save it as %q", kind, path, bare)
			}
			return fmt.Errorf("%s path %q contains `/`, %ss are referenced by bare name (e.g. \"main\", \"sidebar\"), not URL", kind, path, kind)
		}
	}
	return nil
}

// ValidateArtifactPath runs the path checks a save applies to (kind, path): the
// kind-agnostic invariants plus the page-route rules. A rename destination goes
// through it so a move cannot produce a path a save would have refused.
func ValidateArtifactPath(kind, path string) error {
	if err := ValidateFilePath(kind, path); err != nil {
		return err
	}
	if kind == "page" {
		return validatePagePath(path)
	}
	return nil
}

// PathParamNote states, in one place, how this router handles a variable path
// segment for BOTH path types, so page-path and endpoint-path rejections cannot
// teach the model contradictory half-rules. Every rejection that touches the
// distinction appends this note verbatim; it mirrors routing.contract.md, the canonical home.
const PathParamNote = "Route-parameter syntax differs by path type: PAGE routes use Express `:param` (e.g. `/item/:id`); backend endpoint paths are literal and address a row via `/api/<path>/<id>`."

// validatePagePath: must start with '/', not be a reserved homepage alias, use Express-style `:param` (not `[param]`/`{param}`).
func validatePagePath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("page path must start with '/' (got %q)", path)
	}
	if canonical, bad := reservedPagePaths[strings.ToLower(path)]; bad {
		return fmt.Errorf("path %q is reserved, use %q for the homepage", path, canonical)
	}
	// Pages are served at clean URLs, so a `.html`/`.htm` suffix produces a
	// second route for a page that also exists (or will) at its extensionless
	// form - the store keys on the exact path, so the two never reconcile.
	// Steer to the clean form rather than persisting the duplicate.
	if lower := strings.ToLower(path); strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
		clean := path[:strings.LastIndex(lower, ".")]
		return fmt.Errorf("page path %q has a file extension, pages use clean URLs - save it as %q", path, clean)
	}
	if i := strings.Index(path, "["); i >= 0 {
		suggestion := path
		if j := strings.Index(path[i:], "]"); j > 1 {
			seg := path[i+1 : i+j]
			suggestion = path[:i] + ":" + seg + path[i+j+1:]
		}
		return fmt.Errorf("page path %q uses Next.js-style `[%s]` dynamic segments; try %q. %s", path, "param", suggestion, PathParamNote)
	}
	if strings.Contains(path, "{") || strings.Contains(path, "}") {
		return fmt.Errorf("page path %q uses `{param}` dynamic segments. %s", path, PathParamNote)
	}
	// Angle-bracket placeholders (`/<username>`) are NOT a route-param syntax the
	// runtime router understands: stored as a literal, the page 404s while the
	// build still validates clean. Reject with the corrected path; the suggestion
	// rewrites EVERY segment so a multi-param path is fixed in one retry.
	if strings.ContainsAny(path, "<>") {
		suggestion := angleParamRe.ReplaceAllString(path, ":$1")
		return fmt.Errorf("page path %q uses `<param>` placeholder segments; try %q. %s", path, suggestion, PathParamNote)
	}
	return nil
}

// angleParamRe matches an angle-bracket placeholder segment like `<username>` or
// `<post-slug>`, capturing the inner name so it can be rewritten to `:name`.
var angleParamRe = regexp.MustCompile(`<([^/<>]+)>`)
