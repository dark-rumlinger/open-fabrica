package analysis

import (
	"regexp"
	"strings"
)

// JS function analysis for readiness checks: locate every named function / class
// method and report whether its body is empty (the structural signature of an
// unimplemented stub). Comments, string/template contents, AND regex literals are
// masked first so their braces, quotes, and keywords never confuse the scan.

// FuncSpan describes one detected function or method.
type FuncSpan struct {
	Name          string   // declared identifier ("" for anonymous, which is never returned)
	Kind          string   // "function" or "method"
	StartLine     int      // 1-indexed line of the body's opening brace
	EndLine       int      // 1-indexed line of the matching closing brace
	Exported      bool     // top-level `export` (always false for methods)
	BodyEmpty     bool     // body between { } is only whitespace/comments
	Params        []string // top-level parameter tokens in source order (nil for a zero-param list); a destructuring / default / rest parameter is one verbatim token
	UsesArguments bool     // the body references the `arguments` object, so its effective arity is open, not len(Params)
}

var (
	classHeaderRe  = regexp.MustCompile(`(?:^|\W)class\s+([A-Za-z_$][\w$]*)`)
	funcHeaderRe   = regexp.MustCompile(`\bfunction\b\s*\*?\s*([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*$`)
	arrowHeaderRe  = regexp.MustCompile(`(?:\b(?:const|let|var)\s+)?([A-Za-z_$][\w$]*)\s*[:=]\s*(?:async\s+)?\(([^()]*)\)\s*=>\s*$`)
	methodHeaderRe = regexp.MustCompile(`(?:async\s+)?(?:static\s+)?(?:get\s+|set\s+)?\*?\s*([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*$`)
	exportWordRe   = regexp.MustCompile(`(?:^|\W)export(?:\W|$)`)
	argumentsRe    = regexp.MustCompile(`\barguments\b`)
)

// controlKeywords are statements that look like `name(...) {` but open a control
// block, not a function body. Excluded from method/function detection.
var controlKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"with": true, "do": true, "else": true, "function": true, "return": true,
}

// JSFunctions returns every named function and class method in a JS body. Object
// literals, control blocks, and anonymous callbacks are not returned.
func JSFunctions(src string) []FuncSpan {
	mask := maskJS(src)
	lineStarts := computeLineStarts(src)

	var spans []FuncSpan
	depth := 0
	classBodyDepth := map[int]bool{} // depth -> is this brace level a class body?

	for i := 0; i < len(mask); i++ {
		switch mask[i] {
		case '}':
			delete(classBodyDepth, depth)
			if depth > 0 {
				depth--
			}
		case '{':
			kind, name, params, exported := classifyBrace(mask, i, classBodyDepth[depth])
			depth++
			switch kind {
			case braceClass:
				classBodyDepth[depth] = true
			case braceFunc, braceMethod:
				if name == "" || (kind == braceMethod && name == "constructor") {
					break // anonymous, or an empty constructor (a harmless no-op)
				}
				end, empty := matchBrace(mask, i)
				k := "function"
				if kind == braceMethod {
					k = "method"
				}
				spans = append(spans, FuncSpan{
					Name:          name,
					Kind:          k,
					Exported:      exported,
					StartLine:     lineOf(lineStarts, i),
					EndLine:       lineOf(lineStarts, end),
					BodyEmpty:     empty,
					Params:        parseParamList(params),
					UsesArguments: argumentsRe.MatchString(mask[i:end]),
				})
			}
		}
	}
	return spans
}

type braceKind int

const (
	braceOther braceKind = iota
	braceClass
	braceFunc
	braceMethod
)

// classifyBrace inspects the masked header text immediately preceding an opening
// brace to decide what the brace opens. When it opens a function/method it also
// returns the raw parameter text (the bytes between the parens); "" otherwise.
func classifyBrace(mask string, bracePos int, parentIsClassBody bool) (kind braceKind, name, params string, exported bool) {
	// Header = text back to the previous statement boundary.
	start := bracePos - 1
	for start >= 0 {
		if c := mask[start]; c == ';' || c == '{' || c == '}' {
			start++
			break
		}
		start--
	}
	if start < 0 {
		start = 0
	}
	header := strings.TrimSpace(mask[start:bracePos])
	exported = exportWordRe.MatchString(header)

	// Class first: its header ends with `class Name` / `class Name extends X`,
	// never with the `(...)` that func/method/arrow headers require, so this
	// check can't steal one of those.
	if m := classHeaderRe.FindStringSubmatch(header); m != nil {
		return braceClass, m[1], "", false
	}
	if m := funcHeaderRe.FindStringSubmatch(header); m != nil {
		return braceFunc, m[1], m[2], exported
	}
	if m := arrowHeaderRe.FindStringSubmatch(header); m != nil {
		return braceFunc, m[1], m[2], exported
	}
	if parentIsClassBody {
		if m := methodHeaderRe.FindStringSubmatch(header); m != nil && !controlKeywords[m[1]] {
			return braceMethod, m[1], m[2], false
		}
	}
	return braceOther, "", "", false
}

// parseParamList splits a function header's raw parameter text into its top-level
// parameter tokens, trimmed and in source order. A destructuring, default, or rest
// parameter is one verbatim token; an empty or whitespace-only list yields nil.
func parseParamList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, tok := range splitTopLevelCommas(raw) {
		if t := strings.TrimSpace(tok); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// matchBrace returns the index of the brace matching the one at openPos and
// whether the span strictly between them is only whitespace (an empty body).
func matchBrace(mask string, openPos int) (closePos int, empty bool) {
	depth := 0
	empty = true
	for i := openPos; i < len(mask); i++ {
		switch mask[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, empty
			}
		default:
			if i > openPos && !isJSSpace(mask[i]) {
				empty = false
			}
		}
	}
	return len(mask) - 1, empty
}

func isJSSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// maskJS replaces comment characters and string/template interiors with spaces,
// preserving overall length and all newlines so byte offsets and line numbers
// stay valid. Delimiters of strings are kept; their contents are blanked.
// Template literals are interpolation-aware: each `${ ... }` is re-scanned as
// code, which keeps code/string parity correct through nested templates.
func maskJS(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(j int) {
		if out[j] != '\n' {
			out[j] = ' '
		}
	}
	maskCode(src, b, out, blank, 0, len(b), false)
	return string(out)
}

// maskCode masks comments and string/template interiors across b[start:n], leaving
// real code bytes intact. When insideInterp is true it scans a `${ ... }` body and
// returns the index of the closing `}` (nested braces tracked); otherwise it returns n.
func maskCode(src string, b, out []byte, blank func(int), start, n int, insideInterp bool) int {
	depth := 0
	for i := start; i < n; {
		switch {
		case b[i] == '/' && i+1 < n && b[i+1] == '/':
			for i < n && b[i] != '\n' {
				blank(i)
				i++
			}
		case b[i] == '/' && i+1 < n && b[i+1] == '*':
			blank(i)
			blank(i + 1)
			i += 2
			for i < n && !(b[i] == '*' && i+1 < n && b[i+1] == '/') {
				blank(i)
				i++
			}
			if i < n {
				blank(i)
				if i+1 < n {
					blank(i + 1)
				}
				i += 2
			}
		case b[i] == '/' && regexFollows(out[:i]):
			// A regex literal in expression position: blank the whole literal
			// (delimiters, body, flags) like a string so its quotes/braces are inert.
			// If it is not a well-formed single-line regex, regexFollows misjudged a
			// division: treat the `/` as an ordinary code byte and move on.
			if rl := scanRegexLiteral(src, i); rl > 0 {
				for j := i; j < i+rl; j++ {
					blank(j)
				}
				i += rl
			} else {
				i++
			}
		case b[i] == '\'' || b[i] == '"':
			quote := b[i]
			i++ // keep opening delimiter
			for i < n {
				if b[i] == '\\' {
					blank(i)
					if i+1 < n {
						blank(i + 1)
					}
					i += 2
					continue
				}
				if b[i] == quote {
					break
				}
				blank(i)
				i++
			}
			if i < n {
				i++ // keep closing delimiter
			}
		case b[i] == '`':
			i = maskTemplate(src, b, out, blank, i, n)
		case insideInterp && b[i] == '{':
			depth++
			i++
		case insideInterp && b[i] == '}':
			if depth == 0 {
				return i // the `}` that closes this interpolation
			}
			depth--
			i++
		default:
			i++
		}
	}
	return n
}

// maskTemplate masks one template literal beginning at the opening backtick:
// string text is blanked (delimiters kept) and every `${ ... }` interpolation is
// re-scanned as code. Returns the index just past the closing backtick (or n).
func maskTemplate(src string, b, out []byte, blank func(int), start, n int) int {
	i := start + 1 // keep opening backtick
	for i < n {
		if b[i] == '\\' {
			blank(i)
			if i+1 < n {
				blank(i + 1)
			}
			i += 2
			continue
		}
		if b[i] == '`' {
			return i + 1 // keep closing backtick, resume past it
		}
		if b[i] == '$' && i+1 < n && b[i+1] == '{' {
			// Interpolation: keep `${`, scan its code, keep the closing `}`.
			i += 2
			i = maskCode(src, b, out, blank, i, n, true)
			if i < n {
				i++ // step past the `}` (kept as code)
			}
			continue
		}
		blank(i)
		i++
	}
	return n
}

// regexPrecedingKeywords are the keywords after which a `/` opens a regular-
// expression literal rather than a division: each expects an expression next. An
// identifier or numeric literal in that slot means division instead.
var regexPrecedingKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"void": true, "delete": true, "do": true, "else": true, "yield": true,
	"await": true, "case": true, "throw": true,
}

// regexFollows reports whether a `/` at the current scan position begins a regex
// literal rather than a division, judged from the previous significant token: a
// regex follows an operator or expression-expecting keyword, division follows a value.
func regexFollows(code []byte) bool {
	k := len(code) - 1
	for k >= 0 {
		switch code[k] {
		case ' ', '\t', '\n', '\r':
			k--
			continue
		}
		break
	}
	if k < 0 {
		return true // a leading `/` in a module is a regex, never division
	}
	switch code[k] {
	case ')', ']', '"', '\'', '`':
		return false // a value precedes `/`: division
	}
	if isIdentByte(code[k]) {
		end := k + 1
		for k >= 0 && isIdentByte(code[k]) {
			k--
		}
		// A trailing digit makes it a numeric literal (division); otherwise it is an
		// identifier or keyword. Only an expression-expecting keyword allows a regex.
		word := code[k+1 : end]
		if word[0] >= '0' && word[0] <= '9' {
			return false
		}
		return regexPrecedingKeywords[string(word)]
	}
	return true // operator / punctuation: regex position
}

// scanRegexLiteral returns the byte length of the JS regex literal beginning at
// src[start], including flag letters, or 0 if not a well-formed single-line regex.
// Escapes are honored; a `/` inside a `[...]` character class is not the terminator.
func scanRegexLiteral(src string, start int) int {
	n := len(src)
	i := start + 1
	if i >= n || src[i] == '/' || src[i] == '*' {
		return 0 // empty `//`/`/*` are comments, handled before this is reached
	}
	inClass := false
	for i < n {
		switch src[i] {
		case '\\':
			i += 2
			continue
		case '\n':
			return 0 // regex literals do not span lines
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				i++ // consume the closing delimiter
				for i < n && isRegexFlagByte(src[i]) {
					i++
				}
				return i - start
			}
		}
		i++
	}
	return 0 // unterminated
}

func isRegexFlagByte(b byte) bool { return b >= 'a' && b <= 'z' }

// isIdentByte reports whether b can appear in a JS identifier (or numeric literal).
func isIdentByte(b byte) bool {
	return b == '_' || b == '$' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// computeLineStarts returns the byte offset at which each 1-indexed line begins.
// lineStarts[0] is unused; lineStarts[k] is the offset of line k.
func computeLineStarts(src string) []int {
	starts := []int{0, 0} // index 0 unused; line 1 starts at offset 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineOf returns the 1-indexed line containing byte offset off.
func lineOf(lineStarts []int, off int) int {
	// Binary search for the greatest line whose start <= off.
	lo, hi := 1, len(lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lineStarts[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}
