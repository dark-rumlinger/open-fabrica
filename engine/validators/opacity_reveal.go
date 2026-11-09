package validators

// The opacity-reveal validator: catches content that starts invisible
// (`opacity:0`) and is revealed ONLY by a JS-added class fired from an
// IntersectionObserver. The classic shape: `.reveal{opacity:0}` /
// `.reveal.is-in{opacity:1}`, with JS that adds `is-in` when the element scrolls
// into view. When that reveal is deferred (added after an `await fetch`), throws,
// runs before the node exists, or never fires - which it never does for content
// already in the viewport before the observer attaches, or for nodes created
// after it scans - the element stays invisible on first paint. It parses clean,
// boots green, and passes the page-load probe (the probe never scrolls), so only
// the visitor sees a blank screen. This is the animation-hide the visibility_toggle
// gate does not model: that gate only knows display:none / visibility:hidden and
// only reasons about click-toggle controls. FP-averse like the other visibility
// checks - flag only the unconditional `opacity:0` base with no no-JS fallback and
// a JS-observed reveal, so the safe shape (a visible base rule plus opacity:0 on a
// JS-added compound selector like `.reveal.prep`) never trips it. Not blocking (see
// engine/blocking.go):
// a page that mostly works still ships, so it drives a fix-pass.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"openfabrica/store"
)

type opacityRevealValidator struct{}

func (opacityRevealValidator) Name() string { return "opacity_reveal" }

var (
	// opacity:0 (or 0.0), optionally !important, at a declaration boundary. The
	// decl comes from parseCSSRules already lowercased and brace-free.
	orOpacityZeroRe = regexp.MustCompile(`opacity\s*:\s*0(?:\.0+)?\s*(?:!important\s*)?(?:;|$)`)
	// opacity set to a visible (non-zero) value: .5, 0.05, 1, 1.0 - but never 0/0.0.
	orOpacityVisibleRe = regexp.MustCompile(`opacity\s*:\s*(?:0*\.0*[1-9][0-9]*|[1-9][0-9]*(?:\.[0-9]+)?)`)
	orTransformNoneRe  = regexp.MustCompile(`transform\s*:\s*none`)
	orVisibilityVisRe  = regexp.MustCompile(`visibility\s*:\s*visible`)
	// A @media (prefers-reduced-motion) block with its body. Stripped before
	// scanning so its unconditional reveal reset is not read as the no-JS fallback:
	// that reset rescues ONE audience, not the default render.
	orReducedMotionRe = regexp.MustCompile(`(?is)@media[^{]*prefers-reduced-motion[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}`)
)

func (opacityRevealValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	cssFiles, htmlText, jsText, ok := revealContent(pdb)
	if !ok {
		return nil // read error: self-skip, like the other static gates
	}
	// The reveal must be JS-observed. No IntersectionObserver anywhere means no
	// scroll-gated reveal to strand content - nothing to flag.
	if !strings.Contains(jsText, "IntersectionObserver") {
		return nil
	}

	// Aggregate rules across every CSS source (reduced-motion stripped) for reveal
	// and fallback detection; track per-file only for attributing the base rule.
	var allRules []cssRule
	hiddenFile := map[string]string{} // class -> first CSS path defining .class{opacity:0}
	for _, f := range cssFiles {
		clean := orReducedMotionRe.ReplaceAllString(f.body, "")
		rules := parseCSSRules(clean)
		allRules = append(allRules, rules...)
		for c := range loneClassesMatching(rules, orOpacityZeroRe) {
			if _, seen := hiddenFile[c]; !seen {
				hiddenFile[c] = f.path
			}
		}
	}
	if len(hiddenFile) == 0 {
		return nil
	}
	visibleReset := loneClassesMatching(allRules, orOpacityVisibleRe)
	jsAdded := observerAddedClasses(jsText)

	type finding struct{ class, state, file string }
	var found []finding
	for class, file := range hiddenFile {
		// An unconditional visible reset for the same class means the base is not
		// stranded - give it the benefit of the doubt.
		if visibleReset[class] {
			continue
		}
		// The class must actually be used on content, not be dead CSS.
		if !classUsed(class, htmlText, jsText) {
			continue
		}
		// Find a state class that (a) a rule reveals in combination with this class
		// and (b) the JS adds - the reveal-by-JS link.
		var state string
		for _, s := range sortedKeys(revealStateClasses(allRules, class)) {
			if jsAdded[s] {
				state = s
				break
			}
		}
		if state == "" {
			continue
		}
		found = append(found, finding{class: class, state: state, file: file})
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].class < found[j].class
	})

	issues := make([]Issue, 0, len(found))
	for _, f := range found {
		msg := fmt.Sprintf(
			"Elements with class `%s` start at `opacity:0` and are revealed only when JS adds `%s` via an IntersectionObserver (a scroll/observe trigger). "+
				"If that reveal is deferred behind an `await`, throws, runs before the node exists, or never fires for content already in view or for nodes added after the observer scans, the content stays invisible on first paint - and the page-load probe never scrolls, so nothing catches it. "+
				"Make `.%s` visible by default (its base CSS rule is the visible state) and have the deferred module add the hidden state to off-screen elements only (e.g. a `.%s.prep{opacity:0}` class it toggles), so a reveal that never fires leaves content visible instead of blank; never hide hero or first-screen content behind a scroll reveal.",
			f.class, f.state, f.class, f.class)
		issues = append(issues, Issue{
			Severity:      store.SeverityError,
			Category:      "opacity_reveal",
			File:          f.file,
			Deterministic: true,
			Signature:     f.class, // one finding per hidden class
			Message:       msg,
		})
	}
	return issues
}

// revealContent reads the project's CSS (as per-file sources so a finding can name
// the file), plus concatenated HTML document bodies and client JS. CSS = .css files
// plus <style> blocks (attributed to their host page/layout/component); HTML =
// page/layout/component bodies; JS = .js/.mjs files plus <script> blocks. Skips
// platform-managed files. Returns ok=false on a read error so the caller self-skips.
func revealContent(pdb *sql.DB) (css []contentDoc, htmlText, jsText string, ok bool) {
	rows, err := pdb.Query(`SELECT path, COALESCE(body,''), kind, COALESCE(metadata,'')
		FROM of_files WHERE is_deleted = 0 AND kind <> 'test'`)
	if err != nil {
		return nil, "", "", false
	}
	defer rows.Close()
	var htmlB, jsB strings.Builder
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
			htmlB.WriteString(body)
			htmlB.WriteString("\n")
			for i, s := range extractBlocks(body, "style") {
				css = append(css, contentDoc{path: fmt.Sprintf("%s <style#%d>", path, i), kind: kind, body: s})
			}
			for _, s := range extractBlocks(body, "script") {
				jsB.WriteString(s)
				jsB.WriteString("\n")
			}
		case isCSSPath(path):
			css = append(css, contentDoc{path: path, kind: "file", body: body})
		case isJSModuleFile(path):
			jsB.WriteString(body)
			jsB.WriteString("\n")
		}
	}
	if rows.Err() != nil {
		return nil, "", "", false
	}
	return css, htmlB.String(), jsB.String(), true
}

// loneClassesMatching returns the class names whose OWN rule (a lone `.name`
// selector) has a declaration matching declRe.
func loneClassesMatching(rules []cssRule, declRe *regexp.Regexp) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		if !declRe.MatchString(r.decl) {
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

// revealStateClasses returns the OTHER classes that, combined with `class` in a
// selector, carry a declaration that moves the element toward visible
// (`.class.is-in{opacity:1}`, `.class.in{transform:none}`). These are the state
// classes a script would add to reveal the element.
func revealStateClasses(rules []cssRule, class string) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		if !orOpacityVisibleRe.MatchString(r.decl) &&
			!orTransformNoneRe.MatchString(r.decl) &&
			!orVisibilityVisRe.MatchString(r.decl) {
			continue
		}
		for _, part := range strings.Split(r.sel, ",") {
			p := strings.TrimSpace(part)
			if !selectorHasClass(p, class) {
				continue
			}
			for _, c := range tokens(vtSelClassRe, p) {
				if c != class {
					out[c] = true
				}
			}
		}
	}
	return out
}

// observerWindow is how many bytes before a classList call are searched for the
// observer callback that makes it a scroll reveal.
const observerWindow = 300

// observerCueRe marks code that runs because an element scrolled into view: the observed
// entry's `target`, its `isIntersecting` / `intersectionRatio`, or the observer itself.
var observerCueRe = regexp.MustCompile(`\.target|isIntersecting|intersectionRatio|IntersectionObserver|\.unobserve\(`)

// observerAddedClasses returns the literal classes the client JS adds or toggles onto an
// element (classList.add / classList.toggle) from code driven by an IntersectionObserver
// entry. A class added by a pointer or scroll handler (a custom cursor's `is-on`) is not a
// scroll reveal, and counting it flagged a cursor dot that is hidden until the mouse moves.
func observerAddedClasses(jsText string) map[string]bool {
	out := map[string]bool{}
	for _, loc := range vtClassCallRe.FindAllStringSubmatchIndex(jsText, -1) {
		if v := jsText[loc[2]:loc[3]]; v != "add" && v != "toggle" {
			continue
		}
		start := loc[0] - observerWindow
		if start < 0 {
			start = 0
		}
		if observerCueRe.MatchString(jsText[start:loc[0]]) {
			out[jsText[loc[4]:loc[5]]] = true
		}
	}
	return out
}

// classUsed reports whether class `c` appears as a class token in the HTML markup
// or is assigned in the JS (a `className`/`classList`/class-string reference), so
// dead CSS for an unused class is never flagged. Word-bounded to avoid a prefix
// match (`fade` inside `fade-up`).
func classUsed(c, htmlText, jsText string) bool {
	re := regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(c) + `([^\w-]|$)`)
	return re.MatchString(htmlText) || re.MatchString(jsText)
}

func init() {
	RegisterValidator(opacityRevealValidator{})
}
