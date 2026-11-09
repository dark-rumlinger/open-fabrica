package validators

// The visibility-toggle validator: catches a control whose handler RUNS but
// changes nothing the owner sees - the "dead button" that is actually wired. The
// classic shape: an element starts hidden by a class (`.hidden{display:none}`),
// its open handler toggles a DIFFERENT state class (`open`) that has no CSS rule
// showing that element and never removes the hiding class, so `display:none`
// wins and the panel never appears. The dead-control census can't see this (the
// listener exists, so the control reads as alive; the probe never clicks), and
// internal_links only resolves href/action - a class-toggle button is invisible
// to it. Static (of_files): reconcile each element's hide-class against the
// reveal class its JS toggles, against the project CSS. FP-averse like every
// dead-control check - flag only when the reveal is provably inert, never when a
// reveal rule might exist. Not blocking (see engine/blocking.go): a stranded
// panel still ships a usable app, so it drives a fix-pass.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"openfabrica/store"
)

type visibilityToggleValidator struct{}

func (visibilityToggleValidator) Name() string { return "visibility_toggle" }

// qc matches any JS/CSS string quote: ' " or ` (backtick spliced in so this stays
// a valid Go literal).
const qc = "['\"" + "`" + "]"

var (
	// An HTML opening tag: capture the tag name and its raw attribute blob.
	vtOpenTagRe = regexp.MustCompile(`(?is)<([a-zA-Z][\w-]*)\b([^>]*)>`)
	vtIDAttrRe  = regexp.MustCompile(`(?i)\bid\s*=\s*["']([^"']*)["']`)
	vtClassRe   = regexp.MustCompile(`(?i)\bclass\s*=\s*["']([^"']*)["']`)

	// A CSS comment, stripped before rule scanning.
	vtCSSCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// A @keyframes block (with its nested percentage rules), stripped so its
	// `from{}`/`50%{}` inner rules are never read as ordinary selectors.
	vtKeyframesRe = regexp.MustCompile(`(?is)@(?:-\w+-)?keyframes\b[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}`)
	// One CSS rule: selector prelude + declaration block (single brace level;
	// @media wrappers leave harmless noise this scan ignores).
	vtRuleRe = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	// A declaration that hides the element.
	vtHideDeclRe = regexp.MustCompile(`(?is)display\s*:\s*none|visibility\s*:\s*(?:hidden|collapse)`)
	// A declaration that plausibly reveals (used only for a BARE state-class rule).
	vtRevealDeclRe = regexp.MustCompile(`(?is)display\s*:|visibility\s*:|opacity\s*:|transform\s*:|translate|top\s*:|right\s*:|bottom\s*:|left\s*:|inset\s*:|height\s*:|width\s*:|clip`)
	// A declaration that actually UNDOES a hide: only display (a non-none value;
	// none is excluded by the hide check at the call site) or visibility:visible
	// can counter display:none / visibility:hidden.
	vtUnhideDeclRe = regexp.MustCompile(`(?is)display\s*:|visibility\s*:\s*visible`)
	// A lone class selector (`.hidden`), the shape that defines a hide-class.
	vtLoneClassRe = regexp.MustCompile(`^\.(-?[A-Za-z_][\w-]*)$`)
	// Class and id tokens inside a selector part.
	vtSelClassRe = regexp.MustCompile(`\.(-?[A-Za-z_][\w-]*)`)
	vtSelIDRe    = regexp.MustCompile(`#(-?[A-Za-z_][\w-]*)`)

	// A classList add/toggle/remove of a literal class.
	vtClassCallRe = regexp.MustCompile(`classList\.(add|toggle|remove)\(\s*` + qc + `([\w-]+)` + qc)
	vtDotHiddenRe = regexp.MustCompile(`\.hidden\b`)
	// Direct attribute manipulation of an element (removeAttribute('hidden'),
	// toggleAttribute, setAttribute) - a reveal path the class reconciliation must
	// not misread as inert.
	vtAttrRevealRe = regexp.MustCompile(`\.(?:remove|toggle|set)Attribute\(`)
	// The boolean `hidden` HTML attribute (display:none by default). Detected on a
	// tag AFTER its quoted attribute values are masked, so `class="x hidden"`,
	// `aria-hidden`, and `data-hidden` never trip it.
	vtQuotedValRe  = regexp.MustCompile(`(?i)=\s*"[^"]*"|=\s*'[^']*'`)
	vtBareHiddenRe = regexp.MustCompile(`(?i)(^|\s)hidden(\s|/|$)`)
)

// cssRule is one parsed selector prelude + its (lowercased) declaration block.
type cssRule struct {
	sel  string
	decl string
}

func (visibilityToggleValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	cssText, htmlDocs, jsText, ok := visibilityContent(pdb)
	if !ok {
		return nil // read error: self-skip, like the other static gates
	}
	rules := parseCSSRules(cssText)
	hide := hideClasses(rules)
	cssRevealed := cssSelfRevealedClasses(rules)
	// Note: we do NOT bail when len(hide)==0 - an element hidden by the boolean
	// `hidden` attribute is a candidate even in a project with no display:none class.

	type finding struct {
		docPath, docKind, id, hideClass, revealClass, hideKind string
	}
	var found []finding
	seen := map[string]bool{} // docPath\x00id
	for _, doc := range htmlDocs {
		for _, m := range vtOpenTagRe.FindAllStringSubmatch(doc.body, -1) {
			attrs := m[2]
			id := firstSubmatch(vtIDAttrRe, attrs)
			if id == "" {
				continue
			}
			classes := fieldsSet(firstSubmatch(vtClassRe, attrs))
			hc := firstHideClass(classes, hide)
			hiddenAttr := hc == "" && hasHiddenAttr(attrs)
			if hc == "" && !hiddenAttr {
				continue
			}
			// A hide class with its OWN reveal rule elsewhere (the responsive-utility
			// pattern: `.mobile-only{display:none}` plus a media-query rule showing it)
			// is CSS-owned visibility, not a JS toggle contract - never a dead toggle.
			if hc != "" && cssRevealed[hc] {
				continue
			}
			key := doc.path + "\x00" + id
			if seen[key] {
				continue
			}
			adds, reconciled := elementReveal(jsText, id, hc, classes)
			if reconciled || len(adds) == 0 {
				continue // the element's visibility is driven correctly, or never by JS
			}
			addList := sortedKeys(adds)
			inert := true
			for _, c := range addList {
				if hasRevealRule(rules, id, classes, c) {
					inert = false
					break
				}
			}
			if !inert {
				continue // a state class DOES have a rule that reveals this element
			}
			kind := "class"
			if hc == "" {
				kind = "attr"
			}
			seen[key] = true
			found = append(found, finding{doc.path, doc.kind, id, hc, addList[0], kind})
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].docPath != found[j].docPath {
			return found[i].docPath < found[j].docPath
		}
		return found[i].id < found[j].id
	})

	issues := make([]Issue, 0, len(found))
	for _, f := range found {
		var msg string
		if f.hideKind == "attr" {
			msg = fmt.Sprintf(
				"`#%s` starts hidden by the `hidden` attribute (display:none), but the only script that reveals it toggles class `%s`, which has no CSS rule that overrides `hidden` for `#%s` - so the control that opens it fires but nothing appears (a dead button). "+
					"Reveal it by clearing the attribute (`el.hidden = false` or `el.removeAttribute('hidden')`), not by toggling an unrelated class.",
				f.id, f.revealClass, f.id)
		} else {
			msg = fmt.Sprintf(
				"`#%s` is hidden by class `%s` (a `display:none`/`visibility:hidden` rule), but the only script that reveals it toggles class `%s`, which has no CSS rule that shows `#%s` and never removes `%s` - so the control that opens it fires but nothing appears (a dead button). "+
					"Use ONE visibility convention: toggle `%s` itself (`el.classList.toggle('%s')`), or add a CSS rule that makes `.%s` win (e.g. `#%s.%s{display:flex}`) AND drop `%s` from the element's initial markup.",
				f.id, f.hideClass, f.revealClass, f.id, f.hideClass,
				f.hideClass, f.hideClass, f.revealClass, f.id, f.revealClass, f.hideClass)
		}
		is := Issue{
			Severity:      store.SeverityError,
			Category:      "dead_toggle",
			Deterministic: true,
			Signature:     f.id, // one finding per element; Fingerprint is category+page+file+signature
			Message:       msg,
		}
		if f.docKind == "page" {
			is.Page = f.docPath
		} else {
			is.File = f.docPath
		}
		issues = append(issues, is)
	}
	return issues
}

// visibilityContent reads the project's CSS, HTML documents, and client JS from
// of_files in one pass, skipping platform-managed files. CSS = .css files plus
// <style> blocks; HTML = page/layout/component bodies; JS = .js/.mjs files plus
// <script> blocks. Returns ok=false on a read error so the caller self-skips.
func visibilityContent(pdb *sql.DB) (css string, docs []contentDoc, js string, ok bool) {
	rows, err := pdb.Query(`SELECT path, COALESCE(body,''), kind, COALESCE(metadata,'')
		FROM of_files WHERE is_deleted = 0 AND kind <> 'test'`)
	if err != nil {
		return "", nil, "", false
	}
	defer rows.Close()
	var cssB, jsB strings.Builder
	for rows.Next() {
		var path, body, kind, meta string
		if rows.Scan(&path, &body, &kind, &meta) != nil {
			continue
		}
		if body == "" || platformManaged(meta) {
			continue
		}
		switch {
		case kind == "page" || kind == "layout" || kind == "component":
			docs = append(docs, contentDoc{path: path, kind: kind, body: body})
			for _, s := range extractBlocks(body, "style") {
				cssB.WriteString(s)
				cssB.WriteString("\n")
			}
			for _, s := range extractBlocks(body, "script") {
				jsB.WriteString(s)
				jsB.WriteString("\n")
			}
		case isCSSPath(path):
			cssB.WriteString(body)
			cssB.WriteString("\n")
		case isJSModuleFile(path):
			jsB.WriteString(body)
			jsB.WriteString("\n")
		}
	}
	if rows.Err() != nil {
		return "", nil, "", false
	}
	return cssB.String(), docs, jsB.String(), true
}

type contentDoc struct{ path, kind, body string }

func isCSSPath(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".css") }

// extractBlocks returns the inner text of every <tag ...>...</tag> in body.
func extractBlocks(body, tag string) []string {
	re := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>(.*?)</` + tag + `>`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// parseCSSRules splits a CSS body into (selector, declaration) rules, comments
// and @keyframes stripped. Declarations are lowercased for matching.
func parseCSSRules(css string) []cssRule {
	clean := vtCSSCommentRe.ReplaceAllString(css, "")
	clean = vtKeyframesRe.ReplaceAllString(clean, "")
	var out []cssRule
	for _, m := range vtRuleRe.FindAllStringSubmatch(clean, -1) {
		out = append(out, cssRule{sel: strings.TrimSpace(m[1]), decl: strings.ToLower(m[2])})
	}
	return out
}

// hideClasses returns the set of class names whose OWN rule (`.name{...}`, a lone
// class selector) sets display:none or visibility:hidden - the classes that hide
// an element outright.
func hideClasses(rules []cssRule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		if !vtHideDeclRe.MatchString(r.decl) {
			continue
		}
		for _, part := range strings.Split(r.sel, ",") {
			if m := vtLoneClassRe.FindStringSubmatch(strings.TrimSpace(part)); m != nil {
				out[m[1]] = true
			}
		}
	}
	return out
}

// cssSelfRevealedClasses returns the classes that have their OWN lone-class rule
// with a non-hiding display/visibility declaration - the responsive-utility shape
// (`.mobile-only{display:none}` at base, `.mobile-only{display:inline-flex}` inside
// a media query; parseCSSRules captures inner @media rules at the same level).
// An element hidden by such a class has CSS-owned visibility and is never a
// dead-toggle candidate.
func cssSelfRevealedClasses(rules []cssRule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		if vtHideDeclRe.MatchString(r.decl) || !vtUnhideDeclRe.MatchString(r.decl) {
			continue
		}
		for _, part := range strings.Split(r.sel, ",") {
			if m := vtLoneClassRe.FindStringSubmatch(strings.TrimSpace(part)); m != nil {
				out[m[1]] = true
			}
		}
	}
	return out
}

// hasRevealRule reports whether some CSS rule makes state-class `c` reveal the
// element (id + its markup classes). Generous by design (FP-averse): a rule
// applies when its selector references `.c` together with the element's id or one
// of its classes, or is a bare `.c` rule with a visibility-affecting declaration.
// If no such rule exists, toggling `c` is inert for this element.
func hasRevealRule(rules []cssRule, id string, classes map[string]bool, c string) bool {
	for _, r := range rules {
		for _, part := range strings.Split(r.sel, ",") {
			p := strings.TrimSpace(part)
			if !selectorHasClass(p, c) {
				continue
			}
			ids := tokens(vtSelIDRe, p)
			cls := tokens(vtSelClassRe, p)
			if len(ids) > 0 {
				for _, sid := range ids {
					if sid == id {
						return true // #id...c targets this element
					}
				}
				continue // constrained to a different id
			}
			// No id constraint: every non-`c` class in the selector must be one the
			// element actually has (otherwise the rule targets some other element).
			otherClass := false
			ok := true
			for _, cl := range cls {
				if cl == c {
					continue
				}
				otherClass = true
				if !classes[cl] {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if otherClass {
				return true // .someElemClass.c
			}
			if vtRevealDeclRe.MatchString(r.decl) {
				return true // bare .c that actually affects visibility
			}
		}
	}
	return false
}

// elementReveal scans the client JS for how element `id` (hidden by `hideClass`)
// is revealed. It returns the state classes the JS toggles/adds to it (other than
// the hide class), and whether the element's visibility is instead driven
// correctly - the hide class itself toggled, or a direct style/.hidden change -
// in which case it is reconciled and must not be flagged.
func elementReveal(jsText, id, hideClass string, elemClasses map[string]bool) (adds map[string]bool, reconciled bool) {
	adds = map[string]bool{}
	var windows []string

	// Inline reference: `on('id')?.classList.toggle('open')`, `$('#id').classList...`.
	idRe := regexp.MustCompile(qc + `#?` + regexp.QuoteMeta(id) + qc)
	for _, loc := range idRe.FindAllStringIndex(jsText, -1) {
		w := jsText[loc[0]:]
		if len(w) > 200 {
			w = w[:200]
		}
		if i := strings.IndexAny(w, ";\n"); i >= 0 {
			w = w[:i]
		}
		windows = append(windows, w)
	}
	// Var binding: `const drawer = getElementById('id'); ... drawer.classList.toggle('open')`.
	bindRe := regexp.MustCompile(`(?:^|[^.\w$])(?:(?:const|let|var)\s+)?([A-Za-z_$][\w$]*)\s*=\s*[^;\n]*?` + qc + `#?` + regexp.QuoteMeta(id) + qc)
	for _, m := range bindRe.FindAllStringSubmatch(jsText, -1) {
		v := m[1]
		if v == "" {
			continue
		}
		useRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `(?:\?\.|\.)(?:classList\.(?:add|toggle|remove)\([^)]*\)|style\b|hidden\b|(?:remove|toggle|set)Attribute\([^)]*\))`)
		windows = append(windows, useRe.FindAllString(jsText, -1)...)
	}

	for _, w := range windows {
		if strings.Contains(w, ".style") || vtDotHiddenRe.MatchString(w) || vtAttrRevealRe.MatchString(w) {
			reconciled = true
		}
		for _, cm := range vtClassCallRe.FindAllStringSubmatch(w, -1) {
			op, cls := cm[1], cm[2]
			if cls == hideClass {
				reconciled = true // toggling the hide class itself is the correct convention
				continue
			}
			if op == "add" || op == "toggle" {
				adds[cls] = true
			}
		}
	}
	return adds, reconciled
}

// --- small helpers ---------------------------------------------------------

func firstSubmatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

// hasHiddenAttr reports whether a tag's attribute blob carries the boolean
// `hidden` attribute. Quoted values are masked first so `class="x hidden"` and
// `aria-hidden`/`data-hidden` (the `-` before `hidden`) never count.
func hasHiddenAttr(attrs string) bool {
	return vtBareHiddenRe.MatchString(vtQuotedValRe.ReplaceAllString(attrs, " "))
}

func fieldsSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.Fields(s) {
		out[f] = true
	}
	return out
}

func firstHideClass(classes, hide map[string]bool) string {
	// Deterministic: pick the lexically-first class that is a hide class.
	var names []string
	for c := range classes {
		if hide[c] {
			names = append(names, c)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func tokens(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func selectorHasClass(sel, c string) bool {
	re := regexp.MustCompile(`\.` + regexp.QuoteMeta(c) + `(?:[^\w-]|$)`)
	return re.MatchString(sel)
}

func init() {
	RegisterValidator(visibilityToggleValidator{})
}
