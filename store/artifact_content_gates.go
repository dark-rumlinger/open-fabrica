package store

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/dbh"
)

// The save-time content gates: pure functions consulted by ArtifactSave before
// the row is written, each rejecting with a fix-pointing error or returning nil.
// The write-path twin of the engine's post-build static validators: a rule
// enforced here never reaches a fix-pass.

// layoutTokenRe matches any {{...}} placeholder token in a layout body. The
// match group is the token name (with leading/trailing whitespace trimmed).
var layoutTokenRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// validateLayoutBody enforces the layout template contract:
//   - {{content}}      , required, exactly once.
//   - {{assets}}       , optional, zero or one.
//   - {{include:name}} , zero or more (any non-empty name).
//
// Any other {{...}} token is rejected so the LLM learns immediately that it
// invented a placeholder the platform will not substitute.
func validateLayoutBody(body string) error {
	matches := layoutTokenRe.FindAllStringSubmatch(body, -1)
	contentCount := 0
	assetsCount := 0
	for _, m := range matches {
		token := m[1]
		switch {
		case token == "content":
			contentCount++
		case token == "assets":
			assetsCount++
		case strings.HasPrefix(token, "include:") && len(token) > len("include:"):
			// valid, concrete name after the colon
		default:
			return CodedErrorf("layout_unknown_placeholder",
				"unknown layout placeholder {{%s}}. Allowed: {{content}} (required, once), {{assets}} (optional, once), {{include:name}} (any). "+
					"Do not invent new placeholders, global CSS/JS auto-inject before </head> when {{assets}} is absent",
				token,
			)
		}
	}
	if contentCount == 0 {
		return CodedErrorf("layout_missing_content", "layout must contain {{content}} exactly once, the page body substitutes there")
	}
	if contentCount > 1 {
		return CodedErrorf("layout_duplicate_content", "{{content}} appears %d times, only one substitution is supported", contentCount)
	}
	if assetsCount > 1 {
		return CodedErrorf("layout_duplicate_assets", "{{assets}} appears %d times, only one asset sink is supported", assetsCount)
	}
	if err := validateLayoutRawTextContext(body); err != nil {
		return err
	}
	return nil
}

// rawTextOpenRe matches the opening tag of a raw-text element (<style>/<script>).
// Inside these the HTML parser ignores markup until the close tag, so injected
// assets or page content in between would be inert CSS/JS text, not real DOM.
var rawTextOpenRe = regexp.MustCompile(`(?is)<(style|script)\b[^>]*>`)

// validateLayoutRawTextContext rejects a layout whose <style>/<script> elements
// are left unclosed, or that places {{assets}}/{{content}} inside one - silent
// killers that render the page blank; the save-time check makes them actionable.
func validateLayoutRawTextContext(body string) error {
	lower := strings.ToLower(body)
	i := 0
	for i < len(lower) {
		loc := rawTextOpenRe.FindStringSubmatchIndex(lower[i:])
		if loc == nil {
			return nil // no more raw-text elements
		}
		tag := lower[i+loc[2] : i+loc[3]] // "style" or "script"
		afterOpen := i + loc[1]           // index just past the opening tag's '>'
		closeTag := "</" + tag + ">"
		rel := strings.Index(lower[afterOpen:], closeTag)

		end := len(lower)
		if rel >= 0 {
			end = afterOpen + rel
		}
		segment := lower[afterOpen:end]
		var misplaced []string
		if strings.Contains(segment, "{{assets}}") {
			misplaced = append(misplaced, "{{assets}}")
		}
		if strings.Contains(segment, "{{content}}") {
			misplaced = append(misplaced, "{{content}}")
		}
		if len(misplaced) > 0 {
			// Name only the placeholder(s) actually inside this element, and the
			// destination each belongs in, so the fix is unambiguous: {{assets}}
			// goes in <head>, {{content}} in <body>. (Naming both regardless led
			// models to "move" a correctly-placed {{assets}} too.)
			dest := map[string]string{"{{assets}}": "<head>", "{{content}}": "<body>"}
			var moves []string
			for _, p := range misplaced {
				moves = append(moves, p+" into "+dest[p])
			}
			return CodedErrorf("layout_placeholder_in_rawtext",
				"layout places %s inside a <%s> element; it expands to "+
					"<title>/<meta>/<link>/<script> markup, which the browser would treat as inert %s text "+
					"instead of real DOM. Move %s (normal markup, not inside <style>/<script>), "+
					"and close every <%s> with </%s> first",
				strings.Join(misplaced, " and "), tag, rawTextKind(tag), strings.Join(moves, " and "), tag, tag,
			)
		}
		if rel < 0 {
			return CodedErrorf("layout_unclosed_rawtext",
				"layout has an unclosed <%s> element (no matching </%s>); everything after it is swallowed as %s text, "+
					"so injected assets and page content never render. Add the closing </%s> tag",
				tag, tag, rawTextKind(tag), tag,
			)
		}
		i = end + len(closeTag)
	}
	return nil
}

func rawTextKind(tag string) string {
	if tag == "style" {
		return "CSS"
	}
	return "JavaScript"
}

// Inline-CSS budget for a layout's <style> blocks: a minimal reset legitimately
// lives inline, a full design system does not (it belongs in a scope=global CSS
// artifact linked via {{assets}}). Either limit trips the gate.
const (
	maxInlineLayoutCSSBytes = 2048
	maxInlineLayoutCSSLines = 60
)

// validateLayoutInlineCSSBudget rejects a layout that inlines more than a minimal
// reset's worth of CSS: the shared design system must be a scope=global CSS
// artifact auto-injected via {{assets}}. Inlining it in the layout head defeats
// external-CSS caching.
func validateLayoutInlineCSSBudget(body string) error {
	lower := strings.ToLower(body)
	var totalBytes, nonBlankLines int
	i := 0
	for i < len(lower) {
		loc := rawTextOpenRe.FindStringSubmatchIndex(lower[i:])
		if loc == nil {
			break
		}
		tag := lower[i+loc[2] : i+loc[3]] // "style" or "script"
		afterOpen := i + loc[1]           // index just past the opening tag's '>'
		closeTag := "</" + tag + ">"
		rel := strings.Index(lower[afterOpen:], closeTag)
		end := len(lower)
		if rel >= 0 {
			end = afterOpen + rel
		}
		if tag == "style" {
			// Measure the original-case segment (lowercasing preserves offsets).
			segment := strings.TrimSpace(body[afterOpen:end])
			totalBytes += len(segment)
			for _, line := range strings.Split(segment, "\n") {
				if strings.TrimSpace(line) != "" {
					nonBlankLines++
				}
			}
		}
		if rel < 0 {
			break // unclosed <style>/<script> - validateLayoutRawTextContext reports it
		}
		i = end + len(closeTag)
	}

	if totalBytes <= maxInlineLayoutCSSBytes && nonBlankLines <= maxInlineLayoutCSSLines {
		return nil
	}
	return fmt.Errorf(
		"layout inlines %d bytes / %d lines of CSS in its <style> block(s) - over the ~%d-byte / %d-line "+
			"reset budget. The shared design system must be a separate global stylesheet, not inlined in the layout head. "+
			"Move it into a global-scope CSS artifact (the platform links it into <head> via {{assets}}, exactly like js/auth.js):\n"+
			"  manage_files save kind=file scope=global file_path=\"css/app.css\" body=`/* design system */`\n"+
			"Then retry this layout save with only a minimal reset/base block left inline (or none at all); the global CSS auto-injects via {{assets}}",
		totalBytes, nonBlankLines, maxInlineLayoutCSSBytes, maxInlineLayoutCSSLines,
	)
}

// bootstrapWindowGlobals are the auth helpers js/auth.js owns. They are ES-module
// exports, NOT window globals - an inline <script> cannot `import` and can never
// reach them. The former window.__authReady/__user names are kept so a stale read
// is still caught and steered to the import form. Keyed as they appear after `window.`.
var bootstrapWindowGlobals = map[string]bool{
	"__authReady": true, "__user": true, "requireRole": true, "requireAuth": true,
	"redirectIfLoggedIn": true, "apiFetch": true, "logout": true,
}

// inlineScriptBodies returns the concatenated text inside a layout's inline
// <script>...</script> blocks. An external `<script src=...></script>` has no
// inner text and contributes nothing.
func inlineScriptBodies(body string) string {
	lower := strings.ToLower(body)
	var out strings.Builder
	i := 0
	for i < len(lower) {
		loc := rawTextOpenRe.FindStringSubmatchIndex(lower[i:])
		if loc == nil {
			break
		}
		tag := lower[i+loc[2] : i+loc[3]] // "style" or "script"
		afterOpen := i + loc[1]
		closeTag := "</" + tag + ">"
		rel := strings.Index(lower[afterOpen:], closeTag)
		if rel < 0 {
			break // unclosed - validateLayoutRawTextContext reports it
		}
		end := afterOpen + rel
		if tag == "script" {
			out.WriteString(body[afterOpen:end]) // lowercasing preserves offsets
			out.WriteByte('\n')
		}
		i = end + len(closeTag)
	}
	return out.String()
}

// validateLayoutLateGlobals rejects a layout whose inline <script> reads a
// window.* global that only exists AFTER the body-tail JS injection (the auth
// bootstrap's helpers or a sibling global-scope JS file's), so it is undefined
// when the layout script runs and throws on EVERY page. The fix: keep the chrome
// MARKUP in the layout, move its behavior into a global-scope JS artifact.
func validateLayoutLateGlobals(db dbh.Handle, body string) error {
	scripts := inlineScriptBodies(body)
	if strings.TrimSpace(scripts) == "" {
		return nil
	}
	_, consumers := jsWindowProducersConsumers(scripts)
	if len(consumers) == 0 {
		return nil
	}

	// The set of window.* names that load at the body tail: the reserved auth
	// bootstrap helpers, plus whatever existing global-scope JS files define.
	lateGlobals := map[string]string{} // ident -> where it is defined
	for id := range bootstrapWindowGlobals {
		lateGlobals[id] = "js/auth.js (auth bootstrap)"
	}
	if db != nil {
		if rows, err := db.Query(
			`SELECT path, body FROM of_files WHERE kind = 'file' AND is_deleted = 0 AND scope = 'global'
			   AND (path LIKE '%.js' OR path LIKE '%.mjs')`,
		); err == nil {
			for rows.Next() {
				var p, bdy string
				if rows.Scan(&p, &bdy) != nil {
					continue
				}
				producers, _ := jsWindowProducersConsumers(bdy)
				for id := range producers {
					if _, seen := lateGlobals[id]; !seen {
						lateGlobals[id] = p
					}
				}
			}
			_ = rows.Err() // advisory validator input; tolerated like the query error
			rows.Close()
		}
	}

	type offense struct{ ident, src string }
	var bad []offense
	for id := range consumers {
		if src, ok := lateGlobals[id]; ok {
			bad = append(bad, offense{id, src})
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].ident < bad[j].ident })

	var b strings.Builder
	b.WriteString("layout inline <script> reads window.* globals that the platform injects at the END of <body> (with all global and page JS), AFTER the layout's own inline scripts - so they are undefined when the layout script runs and throw on EVERY page:\n")
	for _, o := range bad {
		fmt.Fprintf(&b, "  - window.%s (defined in %s)\n", o.ident, o.src)
	}
	b.WriteString("\nThe layout is markup, not behaviour. Keep the header/nav/footer MARKUP in the layout, but move this script into a LINKED JS module (an inline <script> cannot `import`) - it loads after the DOM and imports the auth helpers by specifier:\n")
	b.WriteString("  manage_files save kind=file scope=global file_path=\"js/chrome.js\" body=`import { requireAuth, logout } from 'auth'; requireAuth().then(function (u) { /* toggle nav, wire logout, etc. */ });`\n")
	b.WriteString("Then retry this layout save with the inline <script> removed. (The auth/realtime/crypto helpers are ES modules imported by specifier - 'auth', 'ofnet', 'e2ee' - never window globals; see the client_js_split guide.)")
	return fmt.Errorf("%s", b.String())
}

// inlineHandlerAttrRe matches an inline event-handler attribute (onclick=,
// onsubmit=, oninput=, ...) and captures its quoted JS expression.
var inlineHandlerAttrRe = regexp.MustCompile(`(?i)\son([a-z]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// handlerBareCallRe matches a bare identifier in call position inside handler
// JS: not preceded by `.` (a member call resolves through its object, not the
// scope chain) or another identifier character.
var handlerBareCallRe = regexp.MustCompile(`(?:^|[^.\w$])([A-Za-z_$][\w$]*)\s*\(`)

// handlerCallKeywords are reserved words that read like a call before a `(`
// (`if (`, `return (`) and never name a binding a handler could miss.
var handlerCallKeywords = map[string]bool{
	"if": true, "else": true, "for": true, "while": true, "do": true,
	"switch": true, "catch": true, "return": true, "function": true,
	"new": true, "delete": true, "typeof": true, "void": true, "await": true,
	"yield": true, "throw": true, "in": true, "of": true, "instanceof": true,
	"with": true, "case": true, "this": true, "super": true,
}

// inlineScriptDeclRe collects top-level declaration names from an inline classic
// <script> block; those live on the page's global scope, so an inline handler CAN
// still reach them. Destructured declarations are deliberately not matched - a
// missed decl false-rejects, but destructured handler functions are not a real
// authoring shape.
var inlineScriptDeclRe = regexp.MustCompile(`(?m)\b(?:function|class|var|let|const)\s+([A-Za-z_$][\w$]*)`)

// validateInlineHandlerScope rejects a page/layout/component body whose inline
// on*= event handler calls a bare identifier that no browser global and no
// same-body inline <script> declares. ALL project JS is served as ES modules
// whose top-level functions never become window globals, so such a handler is
// guaranteed to throw "<name> is not defined" the moment it fires.
func validateInlineHandlerScope(body string) error {
	handlers := inlineHandlerAttrRe.FindAllStringSubmatch(body, -1)
	if len(handlers) == 0 {
		return nil
	}
	declared := map[string]bool{}
	for _, m := range inlineScriptDeclRe.FindAllStringSubmatch(inlineScriptBodies(body), -1) {
		declared[m[1]] = true
	}
	type offense struct{ attr, ident string }
	var bad []offense
	seen := map[string]bool{}
	for _, h := range handlers {
		attr := "on" + strings.ToLower(h[1])
		expr := h[2]
		if expr == "" {
			expr = h[3]
		}
		for _, c := range handlerBareCallRe.FindAllStringSubmatch(expr, -1) {
			name := c[1]
			if handlerCallKeywords[name] || analysis.IsJSGlobal(name) || declared[name] {
				continue
			}
			key := attr + ":" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			bad = append(bad, offense{attr, name})
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Slice(bad, func(i, j int) bool {
		if bad[i].ident != bad[j].ident {
			return bad[i].ident < bad[j].ident
		}
		return bad[i].attr < bad[j].attr
	})
	var b strings.Builder
	b.WriteString("inline event handlers call functions that can never resolve - project JS runs as ES modules, and a module's top-level functions are module-scoped, NOT window globals, so each of these throws \"<name> is not defined\" the moment it fires:\n")
	for _, o := range bad {
		fmt.Fprintf(&b, "  - %s=\"...%s(...)...\"\n", o.attr, o.ident)
	}
	fmt.Fprintf(&b, "\nWire events from the page's JS module instead of the markup: give the element an id (or data- attribute), then in the attached JS file: document.getElementById('...').addEventListener('click', %s). For dynamic lists, delegate from a stable ancestor: container.addEventListener('click', e => { const btn = e.target.closest('[data-action]'); ... }). Retry this save with the on*= attributes removed.", bad[0].ident)
	return fmt.Errorf("%s", b.String())
}
