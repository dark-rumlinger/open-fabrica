package analysis

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"openfabrica/internal/stringutil"
)

// FileTypeForOutline picks the right outline dialect ("css" / "js" / "html")
// based on content-type first, then path extension. Anything unknown falls
// back to the HTML outliner which is the most forgiving.
func FileTypeForOutline(path, contentType string) string {
	lower := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(lower, "text/css"):
		return "css"
	case strings.HasPrefix(lower, "application/javascript"), strings.HasPrefix(lower, "text/javascript"):
		return "js"
	}
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, ".css"):
		return "css"
	case strings.HasSuffix(p, ".js"), strings.HasSuffix(p, ".mjs"):
		return "js"
	}
	return "html"
}

// GenerateOutline produces a line-numbered structural skeleton; bodies collapse to `{ ... }`.
func GenerateOutline(content, fileType string) string {
	switch fileType {
	case "css":
		return outlineCSS(content)
	case "js":
		return outlineJS(content)
	default:
		return outlineHTML(content)
	}
}

// maskCSS blanks `/* */` comment and quoted-string content to spaces, preserving
// length and newlines, so a brace counter sees only real rule-block braces. Same
// byte contract as maskJS, narrowed to CSS. Delimiters kept; interiors blanked.
func maskCSS(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(j int) {
		if out[j] != '\n' {
			out[j] = ' '
		}
	}
	n := len(b)
	for i := 0; i < n; {
		switch {
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
		default:
			i++
		}
	}
	return string(out)
}

// braceCounts returns the number of `{` and `}` on the i-th masked line, or
// (0, 0) when i is out of range. Counting on the masked copy is what keeps a
// brace inside a string/comment/template from shifting block depth.
func braceCounts(masked []string, i int) (opens, closes int) {
	if i < 0 || i >= len(masked) {
		return 0, 0
	}
	return strings.Count(masked[i], "{"), strings.Count(masked[i], "}")
}

// outlineCSS collapses CSS rule bodies, showing selectors with line numbers.
// Brace depth is tracked on a comment/string-masked copy so a brace inside a
// value never miscounts a block.
func outlineCSS(content string) string {
	lines := strings.Split(content, "\n")
	masked := strings.Split(maskCSS(content), "\n")
	var b strings.Builder
	depth := 0
	blockStart := -1
	blockSelector := ""

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		if trimmed == "" {
			continue
		}

		opens, closes := braceCounts(masked, i)

		if depth == 0 && opens > 0 {
			// Entering a new top-level block.
			blockSelector = trimmed
			if closes >= opens {
				// Single-line rule like `.foo { color: red; }`
				fmt.Fprintf(&b, "%d: %s\n", lineNum, trimmed)
			} else {
				blockStart = lineNum
				depth += opens - closes
			}
		} else if depth > 0 {
			depth += opens - closes
			if depth <= 0 {
				// Block closed, emit collapsed summary.
				bodyLines := lineNum - blockStart
				// If the block is short (<=3 lines), show it fully.
				if bodyLines <= 3 {
					for j := blockStart - 1; j <= i && j < len(lines); j++ {
						fmt.Fprintf(&b, "%d: %s\n", j+1, lines[j])
					}
				} else {
					fmt.Fprintf(&b, "%d: %s  <- %d lines\n", blockStart, blockSelector, bodyLines)
				}
				depth = 0
				blockStart = -1
				blockSelector = ""
			}
		} else {
			// Top-level non-block line (comments, imports).
			fmt.Fprintf(&b, "%d: %s\n", lineNum, trimmed)
		}
	}

	fmt.Fprintf(&b, "Lines: %d\n", len(lines))
	return b.String()
}

// outlineJS collapses function/method bodies, showing signatures with line
// numbers. Brace depth is tracked on the maskJS copy so only real code braces
// move it. Functions are surfaced at ANY nesting depth (an IIFE-wrapped module
// keeps inner functions visible); non-function blocks are walked but not printed.
func outlineJS(content string) string {
	lines := strings.Split(content, "\n")
	masked := strings.Split(maskJS(content), "\n")
	var b strings.Builder
	funcCount := 0
	shown := 0
	truncatedAt := -1
	// A backstop, not the primary compactor: run-collapsing (below) does most of
	// the work, so this only trips on pathologically large files. Generous so a
	// normal module's full skeleton always fits.
	const maxOutlineLines = 120

	// emit writes one already-formatted outline line (newline included) unless
	// the cap is reached, in which case it records the first dropped line number
	// for the truncation note. ln is that line, used only for the note.
	emit := func(ln int, s string) {
		if shown >= maxOutlineLines {
			if truncatedAt < 0 {
				truncatedAt = ln
			}
			return
		}
		b.WriteString(s)
		shown++
	}

	clip := stringutil.Truncate

	// Buffer of consecutive plain top-level statements. A run of 3+ collapses to
	// one summary line (1-2 print verbatim), which stops a flat, function-light
	// module from reproducing the whole file as its "outline".
	type stmt struct {
		ln   int
		text string
	}
	var run []stmt
	flushRun := func() {
		switch {
		case len(run) == 0:
			return
		case len(run) <= 2:
			for _, s := range run {
				emit(s.ln, fmt.Sprintf("%d: %s\n", s.ln, s.text))
			}
		default:
			emit(run[0].ln, fmt.Sprintf("%d-%d: ... (%d statements)\n",
				run[0].ln, run[len(run)-1].ln, len(run)))
		}
		run = run[:0]
	}

	// A block tracked for emission. Function blocks always print; non-function
	// blocks print ONLY at the top level - nested non-function blocks are control
	// flow and stay collapsed to avoid clutter.
	type openBlock struct {
		sig     string
		startLn int
		depth   int // brace depth at which this block opened
		isFunc  bool
	}
	var stack []openBlock
	depth := 0

	emitBlock := func(ob openBlock, endLn int) {
		if !ob.isFunc && ob.depth != 0 {
			return
		}
		if ob.isFunc {
			funcCount++
		}
		indent := strings.Repeat("  ", ob.depth)
		bodyLines := endLn - ob.startLn
		if bodyLines <= 3 {
			for j := ob.startLn - 1; j < endLn && j < len(lines); j++ {
				emit(j+1, fmt.Sprintf("%s%d: %s\n", indent, j+1, strings.TrimSpace(lines[j])))
			}
		} else {
			emit(ob.startLn, fmt.Sprintf("%s%d: %s  <- %d lines\n", indent, ob.startLn, ob.sig, bodyLines))
		}
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1
		if trimmed == "" {
			continue
		}

		opens, closes := braceCounts(masked, i)

		switch {
		case opens > closes:
			// Opening a block. Record it so we can emit its range on close.
			flushRun()
			stack = append(stack, openBlock{
				sig:     trimmed,
				startLn: lineNum,
				depth:   depth,
				isFunc:  isFunctionLike(trimmed),
			})
			depth += opens - closes
		case opens < closes:
			flushRun()
			depth += opens - closes
			// Pop every block whose body has now closed (depth fell back to or
			// below the level it opened at).
			for len(stack) > 0 && depth <= stack[len(stack)-1].depth {
				ob := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				emitBlock(ob, lineNum)
			}
			if depth < 0 {
				depth = 0
			}
		default:
			// Balanced line (no net depth change).
			switch {
			case isFunctionLike(trimmed):
				// A single-line function expression / arrow / method.
				flushRun()
				funcCount++
				indent := strings.Repeat("  ", depth)
				emit(lineNum, fmt.Sprintf("%s%d: %s\n", indent, lineNum, clip(trimmed, 100)))
			case depth != 0:
				// Inside a block body: only its function members are surfaced.
			case isTopLevelDecl(trimmed) || strings.HasPrefix(trimmed, "//") ||
				strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*"):
				// Symbol-defining declaration or comment: a structural anchor, kept.
				flushRun()
				emit(lineNum, fmt.Sprintf("%d: %s\n", lineNum, clip(trimmed, 80)))
			default:
				// Plain wiring statement: buffer it for run-collapsing.
				run = append(run, stmt{ln: lineNum, text: clip(trimmed, 80)})
			}
		}
	}
	flushRun()

	if truncatedAt >= 0 {
		fmt.Fprintf(&b, "... (more omitted past line %d)\n", truncatedAt)
	}
	fmt.Fprintf(&b, "Lines: %d | Functions: %d\n", len(lines), funcCount)
	return b.String()
}

// isTopLevelDecl reports whether a balanced top-level line introduces a named
// symbol worth keeping in the skeleton: value declarations (const/let/var) and
// imports. function and class forms are isFunctionLike's job.
func isTopLevelDecl(line string) bool {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "export default ")
	s = strings.TrimPrefix(s, "export ")
	for _, kw := range []string{"const ", "let ", "var ", "import ", "import{", "import*"} {
		if strings.HasPrefix(s, kw) {
			return true
		}
	}
	return false
}

var (
	// `name = function` / `name: function` / `obj.x = async function` - a function
	// assigned to a variable, property, or object key.
	jsAssignFuncRe = regexp.MustCompile(`^[\w$.\[\]'"]+\s*[:=]\s*(async\s+)?function\b`)
	// `name = (args) =>` / `name: arg =>` - an arrow assigned to a name/property.
	jsAssignArrowRe = regexp.MustCompile(`^[\w$.\[\]'"]+\s*[:=]\s*(async\s+)?(\([^)]*\)|[\w$]+)\s*=>`)
	// `const/let/var name = function|(args)=>|arg=>` - a declared function.
	jsDeclFuncRe = regexp.MustCompile(`^(const|let|var)\s+[\w$]+\s*=\s*(async\s+)?(function\b|(\([^)]*\)|[\w$]+)\s*=>)`)
	// Method shorthand `name(args) {` (object/class methods, incl. get/set/async/*).
	jsMethodShortRe = regexp.MustCompile(`^(static\s+|get\s+|set\s+|async\s+|\*\s*)*[\w$]+\s*\([^)]*\)\s*\{`)
	// Keywords that look like a method-shorthand call but are control flow.
	jsCtrlKeywordRe = regexp.MustCompile(`^(if|for|while|switch|catch|return|else|do|with|await|typeof|delete|void|new|throw)\b`)
)

// isFunctionLike checks if a line looks like a function/method/class declaration
// or expression - in declaration, IIFE-wrapper, assigned, property, and method
// shorthand forms. Used to decide which blocks the outline surfaces.
func isFunctionLike(line string) bool {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "export default ")
	s = strings.TrimPrefix(s, "export ")

	if strings.HasPrefix(s, "function ") || strings.HasPrefix(s, "function*") ||
		strings.HasPrefix(s, "function(") || strings.HasPrefix(s, "async function") ||
		strings.HasPrefix(s, "class ") {
		return true
	}

	// IIFE / wrapper lead-ins: `(function`, `!function`, `;(function`, `+function`,
	// `void function`, `(async function`. Strip the wrapper punctuation, then look
	// for a function keyword at the head.
	wrap := strings.TrimLeft(s, "!;+(~ ")
	wrap = strings.TrimPrefix(wrap, "void ")
	if strings.HasPrefix(wrap, "function") || strings.HasPrefix(wrap, "async function") {
		return true
	}

	if jsDeclFuncRe.MatchString(s) || jsAssignFuncRe.MatchString(s) || jsAssignArrowRe.MatchString(s) {
		return true
	}
	if jsMethodShortRe.MatchString(s) && !jsCtrlKeywordRe.MatchString(s) {
		return true
	}
	return false
}

// structuralHTMLTags is the set of element names that define page structure and
// are always surfaced in an HTML outline (lowercase, as the tokenizer reports).
var structuralHTMLTags = map[string]bool{
	"section": true, "div": true, "nav": true, "header": true, "footer": true,
	"main": true, "form": true, "table": true, "ul": true, "ol": true,
	"article": true, "aside": true, "h1": true, "h2": true, "h3": true, "h4": true,
}

// outlineHTML shows the structural/semantic skeleton with line numbers, parsed
// with a real HTML tokenizer so multi-tag lines and markup inside strings are
// handled correctly. Carries each element's id/class and an interactive-element count.
func outlineHTML(content string) string {
	z := html.NewTokenizer(strings.NewReader(content))
	var b strings.Builder
	line := 1
	shown := 0
	const maxOutlineLines = 60
	buttons, forms, inputs := 0, 0, 0
	truncatedAt := -1

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break // includes io.EOF
		}
		startLine := line
		line += bytes.Count(z.Raw(), []byte{'\n'})

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			nameB, hasAttr := z.TagName()
			name := string(nameB)
			switch name {
			case "button":
				buttons++
			case "form":
				forms++
			case "input":
				inputs++
			}
			if !structuralHTMLTags[name] {
				continue
			}
			if shown >= maxOutlineLines {
				if truncatedAt < 0 {
					truncatedAt = startLine
				}
				continue
			}
			fmt.Fprintf(&b, "%d: <%s%s>\n", startLine, name, htmlIDClass(z, hasAttr))
			shown++
		case html.EndTagToken:
			nameB, _ := z.TagName()
			name := string(nameB)
			if !structuralHTMLTags[name] {
				continue
			}
			if shown >= maxOutlineLines {
				if truncatedAt < 0 {
					truncatedAt = startLine
				}
				continue
			}
			fmt.Fprintf(&b, "%d: </%s>\n", startLine, name)
			shown++
		}
	}

	totalLines := strings.Count(content, "\n") + 1
	if truncatedAt >= 0 {
		fmt.Fprintf(&b, "... (more structure omitted past line %d)\n", truncatedAt)
	}
	footer := fmt.Sprintf("Lines: %d", totalLines)
	if buttons > 0 || forms > 0 || inputs > 0 {
		footer += fmt.Sprintf(" | Interactive: %d button(s), %d form(s), %d input(s)", buttons, forms, inputs)
	}
	fmt.Fprintf(&b, "%s\n", footer)
	return b.String()
}

// htmlIDClass drains the current start tag's attributes and returns the id and
// class as a renderable ` id="..." class="..."` suffix (empty when neither is
// present) - the selectors a task targets when wiring against the markup.
func htmlIDClass(z *html.Tokenizer, hasAttr bool) string {
	var id, class string
	for hasAttr {
		k, v, more := z.TagAttr()
		switch string(k) {
		case "id":
			id = string(v)
		case "class":
			class = string(v)
		}
		hasAttr = more
	}
	out := ""
	if id != "" {
		out += fmt.Sprintf(" id=%q", id)
	}
	if class != "" {
		out += fmt.Sprintf(" class=%q", class)
	}
	return out
}
