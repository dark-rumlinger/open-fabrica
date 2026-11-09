package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestValidateLayoutBody_Valid(t *testing.T) {
	cases := []string{
		// Minimal valid layout.
		"<html><body>{{content}}</body></html>",
		// With optional {{assets}} sink.
		"<html><head>{{assets}}</head><body>{{content}}</body></html>",
		// With component includes.
		"<html><body>{{include:header}}{{content}}{{include:footer}}</body></html>",
		// Whitespace around tokens is tolerated.
		"<html><body>{{ content }}</body></html>",
		// Full kitchen sink.
		"<html><head>{{assets}}</head><body>{{include:nav}}{{content}}{{include:footer}}</body></html>",
	}
	for i, body := range cases {
		if err := validateLayoutBody(body); err != nil {
			t.Errorf("case %d: expected valid, got %v (body=%q)", i, err, body)
		}
	}
}

func TestValidateLayoutBody_MissingContent(t *testing.T) {
	err := validateLayoutBody("<html><body></body></html>")
	if err == nil {
		t.Fatal("expected error for missing {{content}}, got nil")
	}
	if !strings.Contains(err.Error(), "{{content}}") {
		t.Errorf("error should mention {{content}}: %v", err)
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "layout_missing_content" {
		t.Errorf("expected coded layout_missing_content (healthy pre-ship catch), got %v", err)
	}
}

func TestValidateLayoutBody_DuplicateContent(t *testing.T) {
	err := validateLayoutBody("<html><body>{{content}}{{content}}</body></html>")
	if err == nil {
		t.Fatal("expected error for duplicate {{content}}, got nil")
	}
	if !strings.Contains(err.Error(), "{{content}}") {
		t.Errorf("error should mention {{content}}: %v", err)
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "layout_duplicate_content" {
		t.Errorf("expected coded layout_duplicate_content, got %v", err)
	}
}

func TestValidateLayoutBody_DuplicateAssets(t *testing.T) {
	err := validateLayoutBody("<html><head>{{assets}}{{assets}}</head><body>{{content}}</body></html>")
	if err == nil {
		t.Fatal("expected error for duplicate {{assets}}, got nil")
	}
	if !strings.Contains(err.Error(), "{{assets}}") {
		t.Errorf("error should mention {{assets}}: %v", err)
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "layout_duplicate_assets" {
		t.Errorf("expected coded layout_duplicate_assets, got %v", err)
	}
}

func TestValidateLayoutBody_UnknownToken(t *testing.T) {
	// Failure mode: the LLM invents an unknown layout token (e.g. {{headers}}),
	// expecting it to be substituted. It isn't, so it must be rejected.
	cases := map[string]string{
		"{{headers}}":          "<html><head>{{headers}}</head><body>{{content}}</body></html>",
		"{{scripts}}":          "<html><body>{{content}}{{scripts}}</body></html>",
		"{{styles}}":           "<html><head>{{styles}}</head><body>{{content}}</body></html>",
		"{{user.name}}":        "<html><body>{{user.name}}{{content}}</body></html>",
		"{{include:}} (empty)": "<html><body>{{include:}}{{content}}</body></html>",
	}
	for label, body := range cases {
		err := validateLayoutBody(body)
		if err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
			continue
		}
		// The error should tell the LLM what IS allowed so it can retry.
		if !strings.Contains(err.Error(), "{{content}}") ||
			!strings.Contains(err.Error(), "{{include:") {
			t.Errorf("%s: error should list allowed tokens: %v", label, err)
		}
		if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "layout_unknown_placeholder" {
			t.Errorf("%s: expected coded layout_unknown_placeholder, got %v", label, err)
		}
	}
}

func TestValidateLayoutBody_AssetsInsideUnclosedStyle(t *testing.T) {
	// "Nothing works" failure mode: the LLM opens a <style> block, puts
	// {{assets}} inside it, and never writes </style>. At render the browser
	// swallows the rest of the document (the injected scripts and page content)
	// as CSS text, so nothing executes.
	body := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <style>
    html, body { height: 100%; }
    {{assets}}
</head>
<body>{{content}}</body>
</html>`
	err := validateLayoutBody(body)
	if err == nil {
		t.Fatal("expected rejection for {{assets}} inside an unclosed <style>, got nil")
	}
	if !strings.Contains(err.Error(), "style") {
		t.Errorf("error should name the <style> element: %v", err)
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "layout_placeholder_in_rawtext" {
		t.Errorf("expected coded layout_placeholder_in_rawtext, got %v", err)
	}
}

// A raw-text element left unclosed with NO placeholder trailing it hits the
// distinct layout_unclosed_rawtext branch (the placeholder-in-rawtext check above
// fires first whenever a placeholder is swallowed).
func TestValidateLayoutBody_PureUnclosedRawText(t *testing.T) {
	err := validateLayoutBody("<html><head>{{assets}}</head><body>{{content}}<script>var x=1;</body></html>")
	if err == nil {
		t.Fatal("expected rejection for an unclosed <script>, got nil")
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "layout_unclosed_rawtext" {
		t.Errorf("expected coded layout_unclosed_rawtext, got %v", err)
	}
}

func TestValidateLayoutBody_NamesOnlyMisplacedPlaceholder(t *testing.T) {
	// {{assets}} is correctly in <head>, but {{content}} is left inside the
	// <head>'s <style> block. The rejection must name only the offending
	// {{content}} (and send it to <body>): naming the correctly-placed {{assets}}
	// too would invite relocating a good sink.
	body := `<!DOCTYPE html>
<html><head>
  {{assets}}
  <style>
    html, body { height: 100%; }
    {{content}}
  </style>
</head><body></body></html>`
	err := validateLayoutBody(body)
	if err == nil {
		t.Fatal("expected rejection for {{content}} inside <style>, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "{{content}}") || !strings.Contains(msg, "<body>") {
		t.Errorf("error should name {{content}} and its <body> destination: %v", err)
	}
	if strings.Contains(msg, "{{assets}}") {
		t.Errorf("error should NOT mention the correctly-placed {{assets}}: %v", err)
	}
}

func TestValidateLayoutBody_UnclosedRawText(t *testing.T) {
	cases := map[string]string{
		"unclosed style":   "<html><head><style>body{}{{assets}}</head><body>{{content}}</body></html>",
		"unclosed script":  "<html><head>{{assets}}<script>var x=1;</head><body>{{content}}</body></html>",
		"content in style": "<html><head>{{assets}}</head><body><style>{{content}}</style></body></html>",
	}
	for label, body := range cases {
		if err := validateLayoutBody(body); err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
		}
	}
}

func TestValidateLayoutBody_BalancedRawTextIsValid(t *testing.T) {
	// Properly closed <style>/<script> with placeholders OUTSIDE them is fine.
	cases := []string{
		"<html><head><style>html,body{height:100%}</style>{{assets}}</head><body>{{content}}</body></html>",
		"<html><head>{{assets}}<script>console.log('ok')</script></head><body>{{content}}</body></html>",
		"<html><head><style>a{}</style><script>1</script>{{assets}}</head><body>{{content}}<script>2</script></body></html>",
	}
	for i, body := range cases {
		if err := validateLayoutBody(body); err != nil {
			t.Errorf("case %d: expected valid, got %v", i, err)
		}
	}
}

func TestValidateLayoutLateGlobals(t *testing.T) {
	// A layout's inline nav-toggle calls window.__authReady, a name an inline
	// <script> can never reach: auth is an ES module, imported by a linked module,
	// and binds no such global. Must reject.
	authToggle := `<!DOCTYPE html><html><head>{{assets}}</head><body>` +
		`<nav><a class="nav-auth" href="/login">Sign in</a><a class="nav-dashboard" style="display:none" href="/dashboard">Dashboard</a></nav>` +
		`{{content}}` +
		`<script>window.__authReady.then(function(u){ /* toggle */ });</script>` +
		`</body></html>`
	err := validateLayoutLateGlobals(nil, authToggle)
	if err == nil {
		t.Fatal("expected rejection for layout inline script using window.__authReady")
	}
	for _, want := range []string{"__authReady", "LINKED JS module", "END of <body>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err.Error())
		}
	}

	// A layout with no inline script, or one that only uses browser-native window
	// fields, is fine.
	ok := []string{
		"<!DOCTYPE html><html><head>{{assets}}</head><body><nav>x</nav>{{content}}</body></html>",
		`<!DOCTYPE html><html><head>{{assets}}</head><body>{{content}}<script>document.title=window.location.pathname;window.scrollTo(0,0);</script></body></html>`,
	}
	for i, body := range ok {
		if err := validateLayoutLateGlobals(nil, body); err != nil {
			t.Errorf("ok case %d wrongly rejected: %v", i, err)
		}
	}
}

func TestValidateLayoutInlineCSSBudget_AllowsResetAndNoStyle(t *testing.T) {
	cases := map[string]string{
		"no style block": "<html><head>{{assets}}</head><body>{{content}}</body></html>",
		"minimal reset": "<html><head><style>" +
			"*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}" +
			":root{--bg:#fff;--text:#111;--accent:#c0593b}" +
			"body{background:var(--bg);color:var(--text);line-height:1.6}" +
			"a{color:var(--accent);text-decoration:none}" +
			"</style>{{assets}}</head><body>{{content}}</body></html>",
	}
	for label, body := range cases {
		if err := validateLayoutInlineCSSBudget(body); err != nil {
			t.Errorf("%s: expected allowed, got %v", label, err)
		}
	}
}

func TestValidateLayoutInlineCSSBudget_RejectsDesignSystem(t *testing.T) {
	// A full design system: many rules pushing well past the line budget.
	var sb strings.Builder
	sb.WriteString("<html><head><style>\n")
	for i := 0; i < 80; i++ {
		sb.WriteString(".component-")
		sb.WriteString(strings.Repeat("x", 3))
		sb.WriteString("{display:flex;align-items:center;padding:1rem;border:1px solid #ddd}\n")
	}
	sb.WriteString("</style>{{assets}}</head><body>{{content}}</body></html>")

	err := validateLayoutInlineCSSBudget(sb.String())
	if err == nil {
		t.Fatal("expected rejection for inlined design system, got nil")
	}
	for _, want := range []string{"global", "css/app.css", "{{assets}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection message missing %q; got: %v", want, err)
		}
	}
}

func TestValidatePageBody_Valid(t *testing.T) {
	cases := []string{
		"<section>hello</section>",
		"<h1>Title</h1><p>body</p>",
		"",
		"<main><article>content</article></main>",
	}
	for i, body := range cases {
		if err := validatePageBody(body); err != nil {
			t.Errorf("case %d: expected valid, got %v", i, err)
		}
	}
}

func TestValidatePageBody_RejectsShell(t *testing.T) {
	cases := map[string]string{
		"DOCTYPE":         "<!DOCTYPE html><section>x</section>",
		"html":            "<html><body><p>x</p></body></html>",
		"body":            "<body><p>x</p></body>",
		"mixed case HTML": "<HTML><p>x</p></HTML>",
	}
	for label, body := range cases {
		err := validatePageBody(body)
		if err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
			continue
		}
		if !strings.Contains(err.Error(), "fragment") {
			t.Errorf("%s: error should mention fragment contract: %v", label, err)
		}
		if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "page_not_fragment" {
			t.Errorf("%s: expected coded page_not_fragment (healthy pre-ship catch), got %v", label, err)
		}
	}
}

// A <body> tag in a page is usually an attempt to style the whole page. A real build
// hit it (a dark full-screen board), found no supported way, and fell back to a
// JS-added body class that lands after first paint. The rejection must name what
// works; a <!DOCTYPE> rejection is a plain misfiled document and stays short.
func TestValidatePageBody_BodyTagTeachesWholePageStyling(t *testing.T) {
	err := validatePageBody(`<body class="board"><div class="board-bg"></div>`)
	if err == nil {
		t.Fatal("a page with a <body> tag must be rejected")
	}
	for _, want := range []string{"body:has(", "root element", "after first paint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection should teach whole-page styling (missing %q): %v", want, err)
		}
	}
	if err := validatePageBody(`<!DOCTYPE html><section>x</section>`); err == nil || strings.Contains(err.Error(), "body:has(") {
		t.Errorf("a <!DOCTYPE> rejection should not carry the body-styling note: %v", err)
	}
}

func TestValidatePageBody_RejectsLayoutPlaceholder(t *testing.T) {
	// {{content}}/{{assets}} are layout scaffolding; a page carrying either is a
	// layout body misfiled as a page (the "wrong slots" scramble). The gate must
	// catch it - the renderer would otherwise emit {{content}} literally, or
	// double-inject the asset block for {{assets}}.
	for label, body := range map[string]string{
		"content":            `<header>{{content}}</header>`,
		"assets":             `<head>{{assets}}</head>`,
		"content spaced":     `<div>{{ content }}</div>`,
		"content among text": `<section><h1>Hi</h1>{{content}}</section>`,
	} {
		err := validatePageBody(body)
		if err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
			continue
		}
		if !strings.Contains(err.Error(), "kind=layout") {
			t.Errorf("%s: error should steer to kind=layout: %v", label, err)
		}
		if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "page_has_layout_token" {
			t.Errorf("%s: expected coded page_has_layout_token (healthy pre-ship catch), got %v", label, err)
		}
	}
	// A page may still use {{include:name}} and other non-layout tokens; the
	// guard is scoped to content/assets so it does not over-match.
	for _, body := range []string{
		`<section>{{include:promo}}</section>`,
		`<p>use {{ include:sidebar }} here</p>`,
		`<section><h1>Hi</h1></section>`,
	} {
		if err := validatePageBody(body); err != nil {
			t.Errorf("fragment %q should be valid: %v", body, err)
		}
	}
}

func TestValidateFilePath_LayoutPathSuggestsBareName(t *testing.T) {
	// A leading/trailing slash on an otherwise-bare name is unambiguous, so the
	// rejection hands back the corrected name (one-retry fix), matching the
	// reject-with-suggestion idiom used for page paths.
	for in, want := range map[string]string{
		"/default": "default",
		"/sidebar": "sidebar",
		"default/": "default",
	} {
		err := ValidateFilePath("layout", in)
		if err == nil {
			t.Errorf("layout path %q should be rejected", in)
			continue
		}
		if !strings.Contains(err.Error(), `"`+want+`"`) {
			t.Errorf("layout path %q: error should suggest %q: %v", in, want, err)
		}
	}
	// A genuinely nested path has no single-segment intent, so it keeps the
	// plain "bare name" rejection with no clean suggestion.
	err := ValidateFilePath("component", "dir/main")
	if err == nil {
		t.Fatal("nested component path should be rejected")
	}
	if strings.Contains(err.Error(), `save it as`) {
		t.Errorf("nested path should not offer a corrected name: %v", err)
	}
}

func TestValidatePagePath_Valid(t *testing.T) {
	cases := []string{"/", "/about", "/blog/post-1", "/deep/nested/path"}
	for _, p := range cases {
		if err := validatePagePath(p); err != nil {
			t.Errorf("path %q should be valid: %v", p, err)
		}
	}
}

func TestValidatePagePath_RejectsReserved(t *testing.T) {
	for _, p := range []string{"/index", "/index.html", "/home", "/home.html", "/INDEX", "/Home.HTML"} {
		err := validatePagePath(p)
		if err == nil {
			t.Errorf("path %q should be rejected", p)
			continue
		}
		if !strings.Contains(err.Error(), `"/"`) {
			t.Errorf("path %q: error should steer to '/': %v", p, err)
		}
	}
}

func TestValidatePagePath_RejectsExtension(t *testing.T) {
	// Non-homepage pages with a .html/.htm suffix steer to the clean form,
	// so /about.html and /about can't both persist as separate routes.
	cases := map[string]string{
		"/about.html":          "/about",
		"/blog/post-1.htm":     "/blog/post-1",
		"/Contact.HTML":        "/Contact",
		"/v1.2/changelog.html": "/v1.2/changelog",
	}
	for p, want := range cases {
		err := validatePagePath(p)
		if err == nil {
			t.Errorf("path %q should be rejected", p)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("path %q: error should steer to %q: %v", p, want, err)
		}
	}
}

func TestValidatePagePath_RequiresSlash(t *testing.T) {
	err := validatePagePath("about")
	if err == nil {
		t.Fatal("expected rejection for missing leading slash")
	}
	if !strings.Contains(err.Error(), "'/'") {
		t.Errorf("error should mention '/': %v", err)
	}
}

func TestValidatePagePath_RejectsAngleBracketParams(t *testing.T) {
	// Angle-bracket placeholders are NOT a route-param syntax the router matches;
	// reject them and suggest the :param form (every segment fixed in one go).
	cases := map[string]string{
		"/<username>":                        "/:username",
		"/<username>/<post-slug>":            "/:username/:post-slug",
		"/<username>/admin/<post-slug>/edit": "/:username/admin/:post-slug/edit",
	}
	for in, want := range cases {
		err := validatePagePath(in)
		if err == nil {
			t.Errorf("path %q should be rejected", in)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("path %q: error should suggest %q, got: %v", in, want, err)
		}
	}
	// A normal path with no angle brackets is still accepted.
	if err := validatePagePath("/:username/:slug"); err != nil {
		t.Errorf("express :param path should be valid: %v", err)
	}
}

func TestValidateNoCrossOriginStylesheets_Rejects(t *testing.T) {
	cases := map[string]string{
		"google fonts https": `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter">`,
		"google fonts http":  `<link rel="stylesheet" href="http://fonts.googleapis.com/x.css">`,
		"protocol-relative":  `<link rel="stylesheet" href="//cdn.example.com/x.css">`,
		"jsdelivr":           `<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/foo/dist/foo.css">`,
		"mixed case rel":     `<link REL="StyleSheet" HREF="https://cdn.example/x.css">`,
		"with integrity":     `<link rel="stylesheet" href="https://cdn.example/x.css" integrity="sha384-abc">`,
	}
	for label, body := range cases {
		err := validateNoCrossOriginStylesheets(body, "layout")
		if err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
			continue
		}
		if !strings.Contains(err.Error(), "@import") {
			t.Errorf("%s: error should point at @import alternative: %v", label, err)
		}
	}
}

func TestValidateNoCrossOriginStylesheets_Allows(t *testing.T) {
	cases := map[string]string{
		"same-origin absolute":    `<link rel="stylesheet" href="/files/site.css">`,
		"same-origin relative":    `<link rel="stylesheet" href="css/site.css">`,
		"localhost bare":          `<link rel="stylesheet" href="http://localhost/x.css">`,
		"icon link cross-origin":  `<link rel="icon" href="https://cdn.example/fav.ico">`,
		"preconnect cross-origin": `<link rel="preconnect" href="https://fonts.googleapis.com">`,
		"manifest cross-origin":   `<link rel="manifest" href="https://cdn.example/manifest.json">`,
		"cross-origin script":     `<script src="https://cdn.jsdelivr.net/npm/foo.js"></script>`,
		"no link tag":             `<html><body>{{content}}</body></html>`,
	}
	for label, body := range cases {
		if err := validateNoCrossOriginStylesheets(body, "layout"); err != nil {
			t.Errorf("%s: expected ok, got %v", label, err)
		}
	}
}

func TestValidateNoCrossOriginScripts_Rejects(t *testing.T) {
	cases := map[string]string{
		"jsdelivr":          `<script src="https://cdn.jsdelivr.net/npm/marked@12/marked.min.js"></script>`,
		"unpkg":             `<script src="https://unpkg.com/three@0.160/build/three.min.js"></script>`,
		"protocol-relative": `<script src="//cdn.example.com/x.js"></script>`,
		"mixed case attrs":  `<SCRIPT SRC="https://cdn.example/x.js"></SCRIPT>`,
		"with integrity":    `<script src="https://cdn.example/x.js" integrity="sha384-abc"></script>`,
		"deferred":          `<script defer src="https://cdn.example/x.js"></script>`,
	}
	for label, body := range cases {
		err := validateNoCrossOriginScripts(body, "layout")
		if err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
			continue
		}
		if !strings.Contains(err.Error(), "external_libraries") {
			t.Errorf("%s: error should point at external_libraries: %v", label, err)
		}
	}
}

func TestValidateNoCrossOriginScripts_Allows(t *testing.T) {
	cases := map[string]string{
		"same-origin absolute":  `<script src="/files/site.js"></script>`,
		"same-origin relative":  `<script src="site.js"></script>`,
		"localhost bare":        `<script src="http://localhost/x.js"></script>`,
		"inline script":         `<script>console.log('ok')</script>`,
		"module no src":         `<script type="module">import 'three';</script>`,
		"importmap":             `<script type="importmap">{"imports":{}}</script>`,
		"no script tag":         `<html><body>{{content}}</body></html>`,
		"cross-origin link tag": `<link rel="stylesheet" href="https://cdn.example/x.css">`,
	}
	for label, body := range cases {
		if err := validateNoCrossOriginScripts(body, "layout"); err != nil {
			t.Errorf("%s: expected ok, got %v", label, err)
		}
	}
}

func TestValidateNoCrossOriginScripts_KindLabel(t *testing.T) {
	body := `<section><script src="https://cdn.example/x.js"></script></section>`
	err := validateNoCrossOriginScripts(body, "page")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !strings.HasPrefix(err.Error(), "page contains") {
		t.Errorf("error should lead with the kind label 'page': %v", err)
	}
}

func TestValidateNoCrossOriginStylesheets_KindLabel(t *testing.T) {
	body := `<section><link rel="stylesheet" href="https://cdn.example/x.css"></section>`
	err := validateNoCrossOriginStylesheets(body, "page")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !strings.HasPrefix(err.Error(), "page contains") {
		t.Errorf("error should lead with the kind label 'page': %v", err)
	}
}

func TestValidateNoCrossOriginStylesheets_ReportsAllOffenders(t *testing.T) {
	body := `<head>
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Fredoka">
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Nunito">
  <link rel="stylesheet" href="https://cdn.example/reset.css">
</head>`
	err := validateNoCrossOriginStylesheets(body, "layout")
	if err == nil {
		t.Fatal("expected rejection")
	}
	for _, want := range []string{"family=Fredoka", "family=Nunito", "cdn.example/reset.css"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q so the model can fix all offenders in one save: %v", want, err)
		}
	}
}

func TestValidateNoSameOriginAssetTags_RejectsHandRolledScriptSrc(t *testing.T) {
	// A page saved with a hand-written <script src="/assets/game.js"> against
	// an asset that was never saved, at a URL prefix the platform doesn't
	// route, must be hard-rejected with a redirect to attach.
	body := `<canvas id="game"></canvas>
<script src="/assets/game.js" type="module"></script>`
	err := validateNoSameOriginAssetTags(body, "page")
	if err == nil {
		t.Fatal("expected rejection of hand-rolled same-origin <script src>")
	}
	if !strings.Contains(err.Error(), "attach") {
		t.Errorf("error should redirect to manage_files attach: %v", err)
	}
	if !strings.Contains(err.Error(), "/assets/game.js") {
		t.Errorf("error should name the offending src so the fix is obvious: %v", err)
	}
	// The rejection carries a failcode so triage can classify it like the other gates.
	if coded, ok := err.(interface{ Code() string }); !ok || coded.Code() != "hand_written_asset_tag" {
		t.Errorf("error should carry code hand_written_asset_tag, got %T %v", err, err)
	}
}

func TestValidateNoSameOriginAssetTags_RejectsHandRolledStylesheetHref(t *testing.T) {
	body := `<link rel="stylesheet" href="/files/css/main.css">
<main>{{content}}</main>`
	err := validateNoSameOriginAssetTags(body, "page")
	if err == nil {
		t.Fatal("expected rejection of hand-rolled same-origin stylesheet")
	}
	if !strings.Contains(err.Error(), "attach") {
		t.Errorf("error should redirect to attach: %v", err)
	}
}

func TestValidateNoSameOriginAssetTags_AllowsInlineAndCrossOrigin(t *testing.T) {
	// Inline <script> blocks (no src), cross-origin tags (handled by the
	// other validator), and <link rel=stylesheet> with cross-origin href
	// must not trip this check.
	body := `<script>console.log("inline ok");</script>
<script src="https://cdn.example.com/lib.js"></script>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter">`
	if err := validateNoSameOriginAssetTags(body, "page"); err != nil {
		t.Errorf("inline/cross-origin tags must pass; cross-origin is the other validator's job: %v", err)
	}
}

func TestValidateNoBlockedIframeDocAccess_RejectsSetAttributeForm(t *testing.T) {
	// The shape to catch: createElement iframe, setAttribute
	// sandbox=allow-scripts (no allow-same-origin), then write to contentDocument.
	body := `var iframe = document.createElement('iframe');
iframe.setAttribute('sandbox', 'allow-scripts');
previewRender.appendChild(iframe);
var doc = iframe.contentDocument || iframe.contentWindow.document;
doc.open(); doc.write(buildPreviewHTML()); doc.close();`
	err := validateNoBlockedIframeDocAccess(body, "script")
	if err == nil {
		t.Fatal("expected rejection of sandboxed iframe + contentDocument access")
	}
	if !strings.Contains(err.Error(), "srcdoc") || !strings.Contains(err.Error(), "allow-same-origin") {
		t.Errorf("error should point to both fixes (srcdoc / allow-same-origin): %v", err)
	}
}

func TestValidateNoBlockedIframeDocAccess_RejectsHTMLAttributeForm(t *testing.T) {
	body := `<iframe sandbox="allow-scripts" id="preview"></iframe>
<script>document.getElementById('preview').contentWindow.document.write(html);</script>`
	if err := validateNoBlockedIframeDocAccess(body, "page"); err == nil {
		t.Fatal("expected rejection of HTML sandbox attr + contentWindow.document access")
	}
}

func TestValidateNoBlockedIframeDocAccess_AllowsSafeForms(t *testing.T) {
	cases := map[string]string{
		"srcdoc only, still sandboxed": `var iframe = document.createElement('iframe');
iframe.setAttribute('sandbox', 'allow-scripts');
iframe.srcdoc = buildPreviewHTML();`,
		"allow-same-origin present": `iframe.setAttribute('sandbox', 'allow-scripts allow-same-origin');
var doc = iframe.contentDocument;`,
		"contentDocument access, no sandbox at all": `var doc = iframe.contentDocument || iframe.contentWindow.document;
doc.write(html);`,
	}
	for name, body := range cases {
		if err := validateNoBlockedIframeDocAccess(body, "script"); err != nil {
			t.Errorf("%s: expected pass, got: %v", name, err)
		}
	}
}

func TestValidateNoCrossOriginAssets_CombinesStyleAndScript(t *testing.T) {
	body := `<!DOCTYPE html><html><head>
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Fredoka">
  <script src="https://cdn.tailwindcss.com/3.4.1"></script>
</head><body>{{content}}</body></html>`
	err := validateNoCrossOriginAssets(body, "layout")
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "fonts.googleapis.com") {
		t.Errorf("combined error should mention the stylesheet offender: %v", err)
	}
	if !strings.Contains(msg, "cdn.tailwindcss.com") {
		t.Errorf("combined error should mention the script offender so the model fixes both classes in one save: %v", err)
	}
}

func TestValidateNoCrossOriginScriptInjection_Rejects(t *testing.T) {
	cases := map[string]string{
		"createElement + .src": `const s = document.createElement('script');
s.src = 'https://cdn.jsdelivr.net/npm/qrcode@1.5.3/build/qrcode.min.js';
document.head.appendChild(s);`,
		"createElement + setAttribute": `const s = document.createElement("script");
s.setAttribute('src', 'https://unpkg.com/qrcode@1.5.3/build/qrcode.min.js');
document.body.appendChild(s);`,
		"static import from URL":   `import { marked } from 'https://cdn.jsdelivr.net/npm/marked@12/marked.esm.js';`,
		"side-effect import":       `import 'https://cdn.example.com/lib.js';`,
		"dynamic import":           `const m = await import('https://cdn.skypack.dev/canvas-confetti');`,
		"protocol-relative import": `import x from '//cdn.example.com/x.mjs';`,
	}
	for label, body := range cases {
		err := validateNoCrossOriginScriptInjection(body, "script")
		if err == nil {
			t.Errorf("%s: expected rejection, got nil", label)
			continue
		}
		if !strings.Contains(err.Error(), "add_library") {
			t.Errorf("%s: error should route to manage_libraries add_library: %v", label, err)
		}
	}
}

func TestValidateNoCrossOriginScriptInjection_Allows(t *testing.T) {
	cases := map[string]string{
		"createElement + local src":      `const s = document.createElement('script'); s.src = '/files/util.js'; document.head.appendChild(s);`,
		"createElement + relative src":   `const s = document.createElement('script'); s.src = 'util.js';`,
		"createElement + localhost src":  `const s = document.createElement('script'); s.src = 'http://localhost/x.js';`,
		"createElement + image src":      `const s = document.createElement('script'); s.src = '/x.js'; img.src = 'https://cdn.example.com/pic.png';`,
		"image src, no script created":   `img.src = 'https://cdn.example.com/pic.webp';`,
		"remote fetch, no script/import": `const r = await fetch('https://api.example.com/data'); const j = await r.json();`,
		"bare specifier import":          `import { camera } from 'three';`,
		"relative import":                `import { state } from './app.state.js';`,
		"scoped bare import":             `import { OrbitControls } from 'three/addons/controls/OrbitControls.js';`,
	}
	for label, body := range cases {
		if err := validateNoCrossOriginScriptInjection(body, "script"); err != nil {
			t.Errorf("%s: expected ok, got %v", label, err)
		}
	}
}

// The cross-origin script gate is wired into ArtifactSave for kind=component too:
// a component body is inlined into a page at render, so a raw CDN <script src> in
// one bypasses the page/layout gate and must be refused before the row is written.
func TestArtifactSave_ComponentRejectsCrossOriginScript(t *testing.T) {
	db := setupArtifactDB(t)
	bad := &Artifact{Kind: "component", Path: "widget",
		Body: `<div class="qr"></div><script src="https://cdn.jsdelivr.net/npm/qrcode@1.5.3/build/qrcode.min.js"></script>`}
	if _, _, err := ArtifactSave(db, bad, "test"); err == nil {
		t.Fatal("expected component save with a cross-origin <script src> to be rejected")
	}
	if ArtifactExists(db, "component", "widget") {
		t.Error("rejected save must not write the row")
	}
	good := &Artifact{Kind: "component", Path: "widget", Body: `<div class="qr"></div>`}
	if _, _, err := ArtifactSave(db, good, "test"); err != nil {
		t.Errorf("a clean component must save: %v", err)
	}
}

// The JS-injection gate is wired into ArtifactSave for kind=file JS: a module that
// hand-injects a CDN script fails only at runtime with no hint to register it, so
// it must be refused at save with the add_library steer.
func TestArtifactSave_FileRejectsCrossOriginScriptInjection(t *testing.T) {
	db := setupArtifactDB(t)
	bad := &Artifact{Kind: "file", Path: "js/qr.js", ContentType: "application/javascript",
		Body: `export function loadQR(){ const s = document.createElement('script'); s.src = 'https://cdn.jsdelivr.net/npm/qrcode@1.5.3/build/qrcode.min.js'; document.head.appendChild(s); }`}
	if _, _, err := ArtifactSave(db, bad, "test"); err == nil {
		t.Fatal("expected file save injecting a cross-origin script to be rejected")
	}
	if ArtifactExists(db, "file", "js/qr.js") {
		t.Error("rejected save must not write the row")
	}
	good := &Artifact{Kind: "file", Path: "js/qr.js", ContentType: "application/javascript",
		Body: `import QRCode from 'qrcode'; export function render(el, text){ QRCode.toCanvas(el, text); }`}
	if _, _, err := ArtifactSave(db, good, "test"); err != nil {
		t.Errorf("a module importing a registered library by bare specifier must save: %v", err)
	}
}

func TestValidateLayoutBody_IncludeVariants(t *testing.T) {
	// Any non-empty name after include: is valid.
	valid := []string{
		"{{include:nav}}",
		"{{include:project-header}}",
		"{{include:big_footer}}",
		"{{include:a}}",
	}
	for _, tok := range valid {
		body := "<html><body>" + tok + "{{content}}</body></html>"
		if err := validateLayoutBody(body); err != nil {
			t.Errorf("include token %q should be valid: %v", tok, err)
		}
	}
}

// setupArtifactDB creates an in-memory project DB with the tables the
// load-order + plan-dedup validators query (of_files + of_runs).
func TestArtifactKindsForPath(t *testing.T) {
	db := setupArtifactDB(t)

	// A page at "/" and an artifact at "js/app.js": each path resolves to one
	// kind. A page and a component sharing the path "shared" simulate the rare
	// cross-kind collision the read handlers must refuse to guess. Insert
	// directly to bypass ArtifactSave's kind-specific path validation.
	save := func(kind, path string) {
		if _, err := db.Exec(
			`INSERT INTO of_files (kind, path, body, is_deleted) VALUES (?, ?, 'x', 0)`,
			kind, path,
		); err != nil {
			t.Fatalf("insert %s/%s: %v", kind, path, err)
		}
	}
	save("page", "/")
	save("file", "js/app.js")
	save("page", "shared")
	save("component", "shared")

	cases := []struct {
		path string
		want []string
	}{
		{"/", []string{"page"}},
		{"js/app.js", []string{"file"}},
		{"shared", []string{"component", "page"}}, // ORDER BY kind
		{"does/not/exist", nil},
	}
	for _, tc := range cases {
		got, err := ArtifactKindsForPath(db, tc.path)
		if err != nil {
			t.Fatalf("ArtifactKindsForPath(%q): %v", tc.path, err)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("path %q: got %v, want %v", tc.path, got, tc.want)
		}
	}

	// A soft-deleted artifact must not surface as a candidate kind.
	if _, err := db.Exec(`UPDATE of_files SET is_deleted = 1 WHERE kind = 'page' AND path = '/'`); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	got, err := ArtifactKindsForPath(db, "/")
	if err != nil {
		t.Fatalf("ArtifactKindsForPath after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("deleted path should resolve to no kinds, got %v", got)
	}
}

func setupArtifactDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY,
			kind TEXT, path TEXT, body TEXT, scope TEXT,
			content_type TEXT, storage TEXT, disk_path TEXT, size INTEGER,
			metadata TEXT DEFAULT '{}', status TEXT,
			is_deleted INTEGER DEFAULT 0, deleted_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(kind, path)
		)`,
		`CREATE TABLE of_file_history (
			id INTEGER PRIMARY KEY,
			file_id INTEGER, kind TEXT, path TEXT,
			version_number INTEGER, changed_by TEXT,
			body_hash TEXT NOT NULL DEFAULT '',
			created_at TEXT DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_snapshot_blobs (
			hash TEXT PRIMARY KEY, body BLOB NOT NULL, size INTEGER NOT NULL,
			refcount INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_runs (
			id TEXT PRIMARY KEY,
			plan_json TEXT,
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func TestArtifactRestoreRevertsToPriorBody(t *testing.T) {
	db := setupArtifactDB(t)
	save := func(body string) int {
		_, v, err := ArtifactSave(db, &Artifact{Kind: "file", Path: "js/app.js", Body: body, ContentType: "application/javascript"}, "brain")
		if err != nil {
			t.Fatalf("save %q: %v", body, err)
		}
		return v
	}
	v1 := save("const a = 1;")
	save("const a = 2;")

	// Restore the explicit first version; the live body must match v1's content
	// and the restore itself becomes a new version.
	newV, err := ArtifactRestore(db, "file", "js/app.js", v1, "admin")
	if err != nil {
		t.Fatalf("restore v%d: %v", v1, err)
	}
	if newV <= 2 {
		t.Fatalf("restore version = %d, want > 2", newV)
	}
	got, _ := ArtifactGet(db, "file", "js/app.js")
	if got.Body != "const a = 1;" {
		t.Fatalf("restored body = %q, want %q", got.Body, "const a = 1;")
	}
}

func TestArtifactRestoreDefaultUndoesLastChange(t *testing.T) {
	db := setupArtifactDB(t)
	_, _, _ = ArtifactSave(db, &Artifact{Kind: "page", Path: "/", Body: "<h1>good</h1>"}, "brain")
	_, _, _ = ArtifactSave(db, &Artifact{Kind: "page", Path: "/", Body: "<h1>oops</h1>"}, "brain")

	// No explicit version: ArtifactRestorableVersion picks the snapshot before the
	// live body, so restoring it undoes the clobber.
	prev := ArtifactRestorableVersion(db, "page", "/")
	if prev != 1 {
		t.Fatalf("restorable version = %d, want 1", prev)
	}
	if _, err := ArtifactRestore(db, "page", "/", prev, "brain"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := ArtifactGet(db, "page", "/")
	if got.Body != "<h1>good</h1>" {
		t.Fatalf("body = %q, want the pre-clobber content", got.Body)
	}
}

func TestArtifactRestoreRecoversDeletedArtifact(t *testing.T) {
	db := setupArtifactDB(t)
	_, _, _ = ArtifactSave(db, &Artifact{Kind: "file", Path: "css/x.css", Body: "body{color:red}", ContentType: "text/css"}, "brain")
	if _, err := ArtifactDelete(db, "file", "css/x.css", "brain"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if a, _ := ArtifactGet(db, "file", "css/x.css"); a != nil {
		t.Fatal("artifact should be soft-deleted")
	}
	// The delete marker carries no snapshot, so the undo target is the last saved body.
	prev := ArtifactRestorableVersion(db, "file", "css/x.css")
	if _, err := ArtifactRestore(db, "file", "css/x.css", prev, "admin"); err != nil {
		t.Fatalf("restore deleted: %v", err)
	}
	got, _ := ArtifactGet(db, "file", "css/x.css")
	if got == nil || got.Body != "body{color:red}" {
		t.Fatalf("deleted artifact was not recovered: %+v", got)
	}
}

func TestBodySnapshotsDedupeAndPrune(t *testing.T) {
	db := setupArtifactDB(t)
	// Identical re-saves must share a single blob (content-addressed dedup).
	for i := 0; i < 3; i++ {
		if _, _, err := ArtifactSave(db, &Artifact{Kind: "file", Path: "js/d.js", Body: "same", ContentType: "application/javascript"}, "brain"); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	var blobs, refcount int
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(refcount),0) FROM of_snapshot_blobs`).Scan(&blobs, &refcount)
	if blobs != 1 || refcount != 3 {
		t.Fatalf("dedup: blobs=%d refcount=%d, want 1 and 3", blobs, refcount)
	}

	// Past MaxBodySnapshots distinct versions, the oldest snapshots are pruned and
	// their orphaned blobs deleted.
	for i := 0; i < MaxBodySnapshots+5; i++ {
		_, _, _ = ArtifactSave(db, &Artifact{Kind: "file", Path: "js/p.js", Body: fmt.Sprintf("v%d", i), ContentType: "application/javascript"}, "brain")
	}
	var snapshots int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_file_history WHERE path = 'js/p.js' AND body_hash <> ''`).Scan(&snapshots)
	if snapshots != MaxBodySnapshots {
		t.Fatalf("retained snapshots = %d, want %d", snapshots, MaxBodySnapshots)
	}
	var orphans int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_snapshot_blobs WHERE refcount <= 0`).Scan(&orphans)
	if orphans != 0 {
		t.Fatalf("orphaned blobs = %d, want 0", orphans)
	}
	// History rows themselves are preserved (attribution), only their snapshot dropped.
	var totalHistory int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_file_history WHERE path = 'js/p.js'`).Scan(&totalHistory)
	if totalHistory != MaxBodySnapshots+5 {
		t.Fatalf("history rows = %d, want %d", totalHistory, MaxBodySnapshots+5)
	}
}

func TestCSSImportURLs(t *testing.T) {
	cases := map[string][]string{
		`@import url('https://fonts.googleapis.com/css2?family=Inter&display=swap');`: {"https://fonts.googleapis.com/css2?family=Inter&display=swap"},
		`@import url("https://example.com/a.css");`:                                   {"https://example.com/a.css"},
		`@import url(https://example.com/b.css);`:                                     {"https://example.com/b.css"},
		`@import "https://example.com/c.css";`:                                        {"https://example.com/c.css"},
		`@import 'https://example.com/d.css';`:                                        {"https://example.com/d.css"},
		`body { color: red; }`:                                                        nil,
		// Two distinct imports; dedup preserves order.
		`@import url('a.css'); @import url('b.css'); @import url('a.css');`: {"a.css", "b.css"},
	}
	for body, want := range cases {
		got := cssImportURLs(body)
		if len(got) != len(want) {
			t.Errorf("body=%q: got %v, want %v", body, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("body=%q: got[%d]=%q, want %q", body, i, got[i], want[i])
			}
		}
	}
}

func TestValidateNoDuplicatePlanImports_Rejects(t *testing.T) {
	db := setupArtifactDB(t)
	planJSON := `{"external_libraries":[{"name":"Press Start 2P","url":"https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap"}]}`
	if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`, planJSON); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	body := `@import url('https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap'); body { font-family: 'Press Start 2P'; }`
	err := validateNoDuplicatePlanImports(db, body, "page")
	if err == nil {
		t.Fatal("expected rejection for duplicate @import, got nil")
	}
	for _, want := range []string{"duplicates plan.external_libraries", "Press Start 2P"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestValidateNoDuplicatePlanImports_AllowsNonPlanURL(t *testing.T) {
	db := setupArtifactDB(t)
	planJSON := `{"external_libraries":[{"name":"Inter","url":"https://fonts.googleapis.com/css2?family=Inter&display=swap"}]}`
	if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`, planJSON); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	// Different URL than the plan entry, should be allowed.
	body := `@import url('https://fonts.googleapis.com/css2?family=Lobster&display=swap');`
	if err := validateNoDuplicatePlanImports(db, body, "page"); err != nil {
		t.Errorf("expected pass for non-duplicate @import, got %v", err)
	}
}

func TestValidateNoDuplicatePlanImports_NoPlan(t *testing.T) {
	db := setupArtifactDB(t)
	// No plan row, validator falls open.
	body := `@import url('https://example.com/a.css');`
	if err := validateNoDuplicatePlanImports(db, body, "page"); err != nil {
		t.Errorf("expected pass without plan, got %v", err)
	}
}

func TestJSWindowProducersConsumers(t *testing.T) {
	body := `
		window.Board = Board;
		window.PIECES = PIECES;
		function init() {
			var x = window.Renderer; // consumer
			window.Phaser.start();   // consumer
		}
		// self-reference; producer subtracts from consumer set
		var b = window.Board;
	`
	producers, consumers := jsWindowProducersConsumers(body)
	if !producers["Board"] || !producers["PIECES"] {
		t.Errorf("producers missing Board/PIECES: %v", producers)
	}
	if !consumers["Renderer"] || !consumers["Phaser"] {
		t.Errorf("consumers missing Renderer/Phaser: %v", consumers)
	}
	if consumers["Board"] {
		t.Errorf("Board should be removed from consumers when self-produced: %v", consumers)
	}
}

func TestIsJSFilePath(t *testing.T) {
	yes := []string{"js/game.js", "foo.JS", "lib/x.mjs", "x.Mjs"}
	for _, p := range yes {
		if !isJSFilePath(p) {
			t.Errorf("expected JS path: %q", p)
		}
	}
	no := []string{"css/style.css", "img/x.png", "data.json", "page.html", ""}
	for _, p := range no {
		if isJSFilePath(p) {
			t.Errorf("expected non-JS path: %q", p)
		}
	}
}
