package validators

// The webfont validator: surfaces a look whose typeface never loads. The CSS
// names a real family, nothing ships it, and the browser silently substitutes
// the OS default - so the project renders in Segoe UI while the plan's art
// direction says otherwise. Nothing looks broken, which is why it survives.
//
// A font reaches the page one of three ways: a font library declared in the plan
// (auto-injected via {{assets}}), an @font-face rule, or a Google/Bunny Fonts
// stylesheet. If none of them carries the family, it is a silent fallback.
//
// Deterministic error-severity so it drives a fix-pass; not in blockingCategories,
// so it never blocks a deploy. Zero-FP by construction: it only names families
// the project itself wrote, and any loading channel at all clears them.

import (
	"context"
	"database/sql"
	"regexp"
	"strings"

	"openfabrica/store"
)

type webfontValidator struct{}

func (webfontValidator) Name() string { return "webfont" }

var (
	// A family list: `font-family: ...` or a `--font*: ...` custom property, which
	// is how a token system names its faces.
	wfFamilyRe = regexp.MustCompile(`(?i)(?:font-family|--[\w-]*font[\w-]*)\s*:([^;}]+)`)
	wfFaceRe   = regexp.MustCompile(`(?i)@font-face`)
	wfHostRe   = regexp.MustCompile(`(?i)fonts\.googleapis\.com|fonts\.bunny\.net|fonts\.gstatic\.com|fontsource`)
	wfCommentR = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// systemFamilies are the families a browser resolves without loading anything,
// plus the generic keywords. Naming only these is a deliberate system stack.
var systemFamilies = map[string]bool{
	"sans-serif": true, "serif": true, "monospace": true, "cursive": true, "fantasy": true,
	"system-ui": true, "ui-sans-serif": true, "ui-serif": true, "ui-monospace": true, "ui-rounded": true,
	"inherit": true, "initial": true, "unset": true, "revert": true, "emoji": true, "math": true,
	"-apple-system": true, "blinkmacsystemfont": true, "segoe ui": true, "roboto": true,
	"helvetica neue": true, "helvetica": true, "arial": true, "arial black": true, "georgia": true,
	"times new roman": true, "times": true, "courier new": true, "courier": true, "consolas": true,
	"menlo": true, "monaco": true, "sfmono-regular": true, "sf mono": true, "trebuchet ms": true,
	"verdana": true, "tahoma": true, "impact": true, "lucida console": true, "lucida grande": true,
	"cambria": true, "palatino": true, "segoe ui emoji": true, "segoe ui symbol": true,
	"apple color emoji": true, "noto color emoji": true, "liberation mono": true, "dejavu sans mono": true,
	// Bundled with Windows or macOS: naming one is a system stack, not a webfont.
	"calibri": true, "candara": true, "constantia": true, "corbel": true, "comic sans ms": true,
	"franklin gothic medium": true, "century gothic": true, "book antiqua": true, "bookman old style": true,
	"garamond": true, "lucida sans": true, "lucida sans unicode": true, "microsoft sans serif": true,
	"palatino linotype": true, "rockwell": true, "segoe print": true, "segoe script": true, "sylfaen": true,
	"avenir": true, "avenir next": true, "seravek": true, "optima": true, "futura": true, "gill sans": true,
	"baskerville": true, "didot": true, "american typewriter": true, "charter": true, "iowan old style": true,
	"papyrus": true, "brush script mt": true, "marker felt": true, "chalkboard se": true, "hoefler text": true,
	"andale mono": true, "big caslon": true, "cochin": true, "copperplate": true, "geneva": true,
	"arial narrow": true, "perpetua": true, "skia": true, "zapfino": true, "new york": true,
}

func (webfontValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	hasUI, cssText, globalPath, ok := webfontContent(pdb)
	if !ok || !hasUI {
		return nil
	}
	clean := wfCommentR.ReplaceAllString(cssText, "")

	// An @font-face rule, or a font host referenced anywhere in the project, ships
	// a face this check cannot attribute to a family. Stay quiet rather than guess.
	text, ok := projectText(pdb)
	if !ok {
		return nil // read error: self-skip like the other static gates
	}
	if wfFaceRe.MatchString(clean) || wfHostRe.MatchString(text) {
		return nil
	}
	// What remains is attributable: a font declared in the plan is auto-injected,
	// and its URL names the family.
	var declared []string
	if plan := latestPlan(pdb); plan != nil {
		for _, lib := range plan.ExternalLibraries {
			if wfHostRe.MatchString(lib.URL) || strings.HasSuffix(strings.ToLower(lib.URL), ".woff2") {
				declared = append(declared, strings.ToLower(lib.Name+" "+lib.URL))
			}
		}
	}

	missing := map[string]bool{}
	for _, m := range wfFamilyRe.FindAllStringSubmatch(clean, -1) {
		for _, part := range strings.Split(m[1], ",") {
			name := strings.Trim(strings.TrimSpace(part), `'"`)
			key := strings.ToLower(name)
			if name == "" || systemFamilies[key] || strings.HasPrefix(name, "var(") || strings.HasPrefix(name, "--") {
				continue
			}
			if !isNameLike(name) || servedBy(key, declared) {
				continue
			}
			missing[name] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := sortedKeys(missing)

	file := globalPath
	if file == "" {
		file = "css"
	}
	msg := "The stylesheet asks for " + strings.Join(quoteAll(names), ", ") +
		", but nothing on the page loads " + pluralThat(len(names)) + ": there is no matching font in the plan's " +
		"`external_libraries`, no `@font-face` rule, and no Google/Bunny Fonts stylesheet. " +
		"The browser silently falls back to the OS default, so the project's typography is not the one the design names. " +
		"Declare the family (`manage_libraries action=add_library` with the " +
		"`https://fonts.googleapis.com/css2?family=<Family+Name>&display=swap` stylesheet URL) or change the CSS to an explicit system stack."

	return []Issue{{
		Severity:      store.SeverityError,
		Category:      "webfont",
		File:          file,
		Deterministic: true,
		Signature:     "silent_fallback",
		Message:       msg,
	}}
}

// servedBy reports whether a declared font library plausibly ships this family.
// Google/Bunny URLs carry the family in `family=Space+Grotesk`, so a space- and
// plus-insensitive containment check resolves it.
func servedBy(family string, declared []string) bool {
	needle := strings.NewReplacer(" ", "", "+", "", "-", "", "_", "").Replace(family)
	if needle == "" {
		return true
	}
	for _, d := range declared {
		if strings.Contains(strings.NewReplacer(" ", "", "+", "", "-", "", "_", "").Replace(d), needle) {
			return true
		}
	}
	return false
}

// isNameLike filters values that are not family names at all (lengths, numbers,
// functions), so a malformed declaration can never invent a finding.
func isNameLike(s string) bool {
	if len(s) < 2 || len(s) > 40 || strings.ContainsAny(s, "()#0123456789") {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

// projectText concatenates every artifact body so a font host referenced from a
// layout, page, or JS module counts as a loading channel.
// webfontContent reads the project's styling surface: whether it has any page, every
// stylesheet and <style> block concatenated, and the path of the global stylesheet (the
// file a finding is attributed to). Platform-managed files are skipped. ok is false on a
// read error so the caller self-skips.
func webfontContent(pdb *sql.DB) (hasUI bool, cssText, globalPath string, ok bool) {
	rows, err := pdb.Query(`SELECT path, COALESCE(body,''), kind, COALESCE(scope,''), COALESCE(metadata,'')
		FROM of_files WHERE is_deleted = 0 AND kind <> 'test'`)
	if err != nil {
		return false, "", "", false
	}
	defer rows.Close()
	var css strings.Builder
	for rows.Next() {
		var path, body, kind, scope, meta string
		if rows.Scan(&path, &body, &kind, &scope, &meta) != nil {
			continue
		}
		if kind == "page" {
			hasUI = true
		}
		if body == "" || platformManaged(meta) {
			continue
		}
		switch {
		case kind == "page" || kind == "layout" || kind == "component":
			for _, s := range extractBlocks(body, "style") {
				css.WriteString(s)
				css.WriteString("\n")
			}
		case isCSSPath(path):
			css.WriteString(body)
			css.WriteString("\n")
			if globalPath == "" && scope == "global" {
				globalPath = path
			}
		}
	}
	if rows.Err() != nil {
		return false, "", "", false
	}
	return hasUI, css.String(), globalPath, true
}

func projectText(pdb *sql.DB) (string, bool) {
	rows, err := pdb.Query(`SELECT COALESCE(body,'') FROM of_files WHERE is_deleted = 0 AND kind <> 'test'`)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var body string
		if rows.Scan(&body) == nil {
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	if rows.Err() != nil {
		return "", false
	}
	return b.String(), true
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = `"` + s + `"`
	}
	return out
}

func pluralThat(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func init() {
	RegisterValidator(webfontValidator{})
}
