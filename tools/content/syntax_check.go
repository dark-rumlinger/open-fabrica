package content

// Post-edit syntax gate for code artifacts. save / patch / bulk_replace
// compute a new body in memory and would otherwise persist it
// unconditionally, so a write that produces invalid code saves cleanly and only
// surfaces stages later in the runtime probe. This gate parses the edited body
// first (via the shared analysis.JSSyntax parser, which the readiness validator also
// uses) and refuses any write that takes a file from parsing clean to not
// parsing, handing the location back to the caller instead.

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"openfabrica/analysis"
	"openfabrica/tools"
)

// exportListFuncRe matches an ES `export { ... }` list that reaches the `function`
// keyword before its closing brace - a function DECLARATION placed inside the
// named-export list (`export { a, function getX(){...} }`), which is a SyntaxError.
// `[^}]*` stops at the first `}`, so a well-formed `export { a, b }` followed by a
// separate `function foo(){}` does NOT match - only the genuinely invalid shape.
var exportListFuncRe = regexp.MustCompile(`export\s*\{[^}]*\bfunction\b`)

// apostropheBreakRe matches a single-quoted string immediately followed by a
// letter - the shape a mid-string apostrophe leaves behind (`'It'll` parses as
// `'It'` then a bare `ll`), which the parser reports only as a bewildering
// `Expected ";" but found "ll"`.
var apostropheBreakRe = regexp.MustCompile(`'[^'\n]*'[A-Za-z]`)

// detailLineRe extracts the 1-based line number from a parser detail of the
// shape "message (line:col)".
var detailLineRe = regexp.MustCompile(`\((\d+):\d+\)`)

// syntaxHint returns a targeted one-line fix to append to a rejection detail for a
// high-signal, recurring authoring mistake, or "" when no known pattern matches. It
// is pattern-gated (and cross-checked against the parser's "but found" shape) and
// additive, so an unrelated syntax error reads exactly as it did before.
func syntaxHint(newBody, detail string) string {
	if !strings.Contains(detail, "but found") {
		return ""
	}
	if exportListFuncRe.MatchString(newBody) {
		return " A function declaration inside an `export { ... }` list is invalid - " +
			"declare it first (function getX(){...}) then export it by name (export { getX })."
	}
	if apostropheBreakRe.MatchString(detailLine(newBody, detail)) {
		return " An apostrophe inside a single-quoted string terminates it early ('It'll reads as 'It' then a bare ll) - " +
			"build copy and HTML strings with double quotes or template literals, or escape it ('It\\'ll')."
	}
	return ""
}

// detailLine returns the body line a parser detail points at ("" when the
// detail carries no location), so a hint pattern-matches the offending line
// rather than the whole body.
func detailLine(body, detail string) string {
	m := detailLineRe.FindStringSubmatch(detail)
	if m == nil {
		return ""
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return ""
	}
	lines := strings.Split(body, "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// syntaxResult is one cached codeSyntax verdict.
type syntaxResult struct {
	gated, clean bool
	detail       string
}

// codeSyntax is a pure function of (kind, path, body), but a single write re-derives
// the same body several times - the write gate, each import-heal re-check, and the
// parse-status echo - and each derivation is an esbuild parse. A content-addressed
// cache collapses those to one parse per distinct body. An entry is keyed by a hash
// of its inputs, so it is never stale; two generations bound the memory, the older
// cleared when the newer fills. The lock is held only around the maps, never across
// the parse, so parses for different projects still run concurrently.
var (
	syntaxCacheMu   sync.Mutex
	syntaxCacheCur  = map[[32]byte]syntaxResult{}
	syntaxCachePrev = map[[32]byte]syntaxResult{}
)

// syntaxCacheMax bounds one generation; at the cap the current map becomes prev and a
// fresh current starts, so at most 2*max entries (each a hash plus a small verdict)
// are held.
const syntaxCacheMax = 512

func syntaxCacheKey(kind, path, body string) [32]byte {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(body))
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// codeSyntax reports a code artifact's syntax health for the write gate and the
// inline parse-status echo, memoized by content (see syntaxResult). The computation
// dispatches on artifact type in computeCodeSyntax.
func codeSyntax(kind, path, body string) (gated, clean bool, detail string) {
	key := syntaxCacheKey(kind, path, body)
	syntaxCacheMu.Lock()
	if r, ok := syntaxCacheCur[key]; ok {
		syntaxCacheMu.Unlock()
		return r.gated, r.clean, r.detail
	}
	if r, ok := syntaxCachePrev[key]; ok {
		syntaxCacheCur[key] = r // promote so a still-live body survives the next swap
		syntaxCacheMu.Unlock()
		return r.gated, r.clean, r.detail
	}
	syntaxCacheMu.Unlock()

	gated, clean, detail = computeCodeSyntax(kind, path, body)

	syntaxCacheMu.Lock()
	if len(syntaxCacheCur) >= syntaxCacheMax {
		syntaxCachePrev, syntaxCacheCur = syntaxCacheCur, map[[32]byte]syntaxResult{}
	}
	syntaxCacheCur[key] = syntaxResult{gated: gated, clean: clean, detail: detail}
	syntaxCacheMu.Unlock()
	return gated, clean, detail
}

// computeCodeSyntax dispatches on artifact type: a JS-family file is fully parsed
// (esbuild), a CSS file is checked for block-brace balance, and an HTML-bodied
// artifact (page/layout/component) has its executable inline <script> blocks parsed.
// gated is false for anything none of these cover, so callers skip gating then.
func computeCodeSyntax(kind, path, body string) (gated, clean bool, detail string) {
	if r := analysis.JSSyntax(path, body); r.Gated {
		return true, r.Clean, r.Detail
	}
	if r := analysis.CSSSyntax(path, body); r.Gated {
		return true, r.Clean, r.Detail
	}
	// HTML-bodied artifacts carry their JS in inline <script> blocks, which the
	// path-keyed JS check above never sees (the path is a route, not a *.js file).
	if isHTMLBodyKind(kind) {
		return inlineScriptSyntax(body)
	}
	return false, false, ""
}

// isHTMLBodyKind reports whether an artifact kind stores an HTML document body
// (rather than a standalone code file), so its inline <script> blocks are the JS
// to syntax-check.
func isHTMLBodyKind(kind string) bool {
	switch kind {
	case "page", "layout", "component":
		return true
	}
	return false
}

// inlineScriptSyntax parses every executable inline <script> in an HTML body and
// reports the first block with a syntax error, at a document-relative line. It is
// NOT gated when the body has no executable inline script (nothing to parse), so a
// scriptless page is neither rejected nor echoed as parsed - and a page that had
// no script but gains a broken one reads as clean->broken and is correctly caught.
func inlineScriptSyntax(body string) (gated, clean bool, detail string) {
	scripts := analysis.InlineScripts(body)
	if len(scripts) == 0 {
		return false, false, ""
	}
	for _, s := range scripts {
		if r := analysis.JSSyntax("inline.js", s.Code); r.Gated && !r.Clean {
			docLine := s.StartLine + r.FirstLine - 1
			return true, false, fmt.Sprintf("inline <script> has %s (document line %d)", r.Detail, docLine)
		}
	}
	return true, true, ""
}

// rejectIfBrokeSyntax guards a code-artifact write. If oldBody was clean but the
// edited newBody is not, this edit introduced the breakage, so it returns a failed
// Result and the caller must leave the stored artifact untouched. It returns nil when
// the path is not gated, the edit changed nothing, or the file was already broken
// before the edit, so an in-progress repair on a broken file is never trapped.
//
// exists reports whether a stored artifact already lives at this path, so the recovery
// hint matches the caller's real next step. On an edit of an existing artifact, patch
// stay valid, so the hint is "adjust your edit and retry". On a save that
// was CREATING a new file, the rejection stored nothing, so a follow-up patch only
// bounces off notFoundForModify; the hint instead steers back to a corrected full-body
// save, removing the create-rejected -> patch -> not-found detour that otherwise costs
// two wasted iterations.
func rejectIfBrokeSyntax(kind, path, oldBody, newBody string, exists bool) *tools.Result {
	if newBody == oldBody {
		return nil
	}
	gated, clean, detail := codeSyntax(kind, path, newBody)
	if !gated || clean {
		return nil
	}
	if oldGated, oldClean, _ := codeSyntax(kind, path, oldBody); oldGated && !oldClean {
		// The file was already broken before this edit, so a partial repair that is
		// still broken must not be trapped. A NOT-gated old body (e.g. a page that
		// had no inline <script>) is treated as clean, so adding a broken script is
		// correctly rejected as clean->broken rather than waved through.
		return nil
	}
	detail += syntaxHint(newBody, detail)
	if !exists {
		return tools.FailCode("syntax_error",
			"save rejected: %s would have a syntax error: %s. "+
				"The file was NOT created, so there is nothing to patch yet - fix the body and call save again with the full corrected content. "+
				"patch only modifies an already-saved artifact.",
			path, detail)
	}
	return tools.FailCode("syntax_error",
		"edit rejected: would introduce a syntax error in %s: %s. "+
			"The artifact was not modified, adjust your edit and retry.",
		path, detail)
}

// addParseStatus records the final body's syntax health on a successful write so
// the model gets the same signal a separate `check` call would return, inline -
// removing the reflexive save -> check round trip from the build loop. The write
// gate (rejectIfBrokeSyntax) refuses a clean->broken edit but lets a still-broken
// repair through, so a successful write does not by itself prove the file is sound;
// this reports the actual result of what was stored. No-op for non-gated artifacts
// (an HTML body with no inline <script>, and other non-code kinds), so parsed_clean
// is only ever set for the JS family, CSS, and pages carrying inline scripts.
func addParseStatus(data map[string]any, kind, path, body string) {
	gated, clean, detail := codeSyntax(kind, path, body)
	if !gated {
		return
	}
	if clean {
		data["parsed_clean"] = true
		return
	}
	data["parsed_clean"] = false
	data["parse_error"] = detail
}
