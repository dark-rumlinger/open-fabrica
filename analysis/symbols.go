package analysis

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"openfabrica/internal/stringutil"
)

// Symbol extraction for the persistent symbol index, reusing the same low-level
// primitives the on-demand text outline uses. It deliberately does NOT touch the
// GenerateOutline renderers: the text outline is a pinned human-facing surface,
// and sharing the underlying parsers keeps the two from drifting on what a symbol is.

// Symbol is one defined name (a function, method, export, CSS selector, or HTML
// id/class) with the source location and a one-line signature a caller can show
// without reading the body.
type Symbol struct {
	Name      string
	Kind      string // js_func | js_method | js_export | css_selector | html_id | html_class
	Line      int    // 1-indexed
	Signature string
	Exported  bool
}

// SymbolRef is one usage edge: a reference to Name made from the file being
// extracted. Spec is the raw module specifier for an import (resolved to a target
// path by the indexer); RefKind labels the edge ("import").
type SymbolRef struct {
	Name    string
	Line    int
	RefKind string
	Spec    string
}

const maxSignatureLen = 200

// ExtractSymbols dispatches on the outline dialect (css / js / html, from
// FileTypeForOutline) and returns the file's defined symbols and reference edges.
func ExtractSymbols(content, fileType string) ([]Symbol, []SymbolRef) {
	switch fileType {
	case "css":
		return extractCSSSymbols(content), nil
	case "js":
		return extractJSSymbols(content)
	default:
		return extractHTMLSymbols(content)
	}
}

// extractJSSymbols surfaces every named function/method, the named exports not
// already one of those functions, the named-import edges, and call-site usage
// edges. Anonymous spans are dropped by JSFunctions before they reach here.
func extractJSSymbols(content string) ([]Symbol, []SymbolRef) {
	lines := strings.Split(content, "\n")
	var syms []Symbol
	funcNames := map[string]bool{}
	// defSites records where each name is DEFINED in this file (by body-brace
	// line), so a definition header like `function foo(` is never counted as a
	// call to foo - extractJSCallRefs skips a usage that lands on a def line.
	defSites := map[string]map[int]bool{}
	for _, fs := range JSFunctions(content) {
		if fs.Name == "" {
			continue
		}
		kind := "js_func"
		if fs.Kind == "method" {
			kind = "js_method"
		}
		syms = append(syms, Symbol{
			Name:      fs.Name,
			Kind:      kind,
			Line:      fs.StartLine,
			Signature: jsSignatureAt(lines, fs.StartLine),
			Exported:  fs.Exported,
		})
		funcNames[fs.Name] = true
		if defSites[fs.Name] == nil {
			defSites[fs.Name] = map[int]bool{}
		}
		defSites[fs.Name][fs.StartLine] = true
	}
	// A name exported by declaration (export const/let/var, re-export list) that is
	// not already a function/method def - e.g. an exported constant or object.
	for _, name := range ExtractNamedExports(content) {
		if funcNames[name] {
			continue
		}
		syms = append(syms, Symbol{Name: name, Kind: "js_export", Signature: "export " + name, Exported: true})
	}

	var refs []SymbolRef
	for _, ni := range ExtractNamedImports(content) {
		refs = append(refs, SymbolRef{Name: ni.Imported, RefKind: "import", Spec: ni.Spec})
	}
	refs = append(refs, extractJSCallRefs(content, defSites)...)
	return syms, refs
}

// jsCallRe lexically matches an identifier immediately followed by an opening
// paren - a call or construction site - with an optional leading dot so method
// calls (`obj.render(`) are captured too. Run against the comment/string-masked
// body so a `(` inside a string or comment never produces a phantom edge.
var jsCallRe = regexp.MustCompile(`(\.)?([A-Za-z_$][\w$]*)\s*\(`)

// jsCallStopwords are keywords that look like a call (`if (`, `for (`, `switch (`)
// but open a control construct, not a usage. Excluded so they never become refs.
var jsCallStopwords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"with": true, "do": true, "else": true, "return": true, "function": true,
	"typeof": true, "instanceof": true, "void": true, "delete": true, "await": true,
	"yield": true, "throw": true, "case": true, "in": true, "of": true, "super": true,
}

// extractJSCallRefs returns one "call" usage edge per call/construction site that
// is not a control keyword, a declaration header, or the symbol's own definition
// line. Best-effort lexical pass: a bare method header can still read as a call,
// which only adds a self-referential edge in the defining file, never a wrong cross-file one.
func extractJSCallRefs(content string, defSites map[string]map[int]bool) []SymbolRef {
	mask := maskJS(content)
	lineStarts := computeLineStarts(mask)
	seen := map[string]bool{} // dedup (name|line) so a name called twice on a line yields one edge
	var refs []SymbolRef
	for _, m := range jsCallRe.FindAllStringSubmatchIndex(mask, -1) {
		dotted := m[2] >= 0
		nameStart, nameEnd := m[4], m[5]
		name := mask[nameStart:nameEnd]
		if jsCallStopwords[name] {
			continue
		}
		// Skip a named-function declaration header (`function foo(`): the preceding
		// word is `function`. Member calls (dotted) are never declarations.
		if !dotted && precedingWord(mask, nameStart) == "function" {
			continue
		}
		line := lineOf(lineStarts, nameStart)
		if d := defSites[name]; d != nil && d[line] {
			continue // the symbol's own definition site, not a use of it
		}
		key := fmt.Sprintf("%s\x00%d", name, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, SymbolRef{Name: name, RefKind: "call", Line: line})
	}
	return refs
}

// precedingWord returns the identifier word immediately before pos (skipping
// whitespace), or "" when the preceding character is not part of a word. Used to
// tell a `function name(` declaration from a `name(` call.
func precedingWord(mask string, pos int) string {
	j := pos - 1
	for j >= 0 && isJSSpace(mask[j]) {
		j--
	}
	end := j + 1
	for j >= 0 && isWordByte(mask[j]) {
		j--
	}
	if j+1 >= end {
		return ""
	}
	return mask[j+1 : end]
}

// isWordByte reports whether b can appear in a JS identifier.
func isWordByte(b byte) bool {
	return b == '_' || b == '$' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// jsSignatureAt returns the trimmed header line up to the opening brace - the
// function/method signature without its body.
func jsSignatureAt(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	s := strings.TrimSpace(lines[line-1])
	if i := strings.Index(s, "{"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return truncateSignature(s)
}

// extractCSSSymbols collects the selector of every rule block (top-level and
// nested inside @media/@supports), line-numbered. Brace depth is read off the
// comment/string-masked copy (maskCSS) so a brace inside a value never miscounts
// a block; @-rule headers (@media, @keyframes) are skipped as non-selectors.
func extractCSSSymbols(content string) []Symbol {
	lines := strings.Split(content, "\n")
	masked := strings.Split(maskCSS(content), "\n")
	var syms []Symbol
	for i, line := range lines {
		opens, _ := braceCounts(masked, i)
		if opens == 0 {
			continue // not a block-opening line
		}
		sel := strings.TrimSpace(line)
		if bi := strings.Index(sel, "{"); bi >= 0 {
			sel = strings.TrimSpace(sel[:bi])
		}
		if sel == "" || strings.HasPrefix(sel, "@") {
			continue
		}
		for _, one := range strings.Split(sel, ",") {
			one = strings.TrimSpace(one)
			if one == "" {
				continue
			}
			syms = append(syms, Symbol{Name: one, Kind: "css_selector", Line: i + 1, Signature: truncateSignature(one)})
		}
	}
	return syms
}

// extractHTMLSymbols walks the markup with the same tokenizer the HTML outline
// uses and emits an html_id symbol per id and an html_class symbol per class on
// each structural element (the selectors a task wires against). Line numbers are
// the start line of the tag, tracked by counting newlines in each raw token.
func extractHTMLSymbols(content string) ([]Symbol, []SymbolRef) {
	z := html.NewTokenizer(strings.NewReader(content))
	var syms []Symbol
	line := 1
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break // includes io.EOF
		}
		startLine := line
		line += bytes.Count(z.Raw(), []byte{'\n'})
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		nameB, hasAttr := z.TagName()
		name := string(nameB)
		if !structuralHTMLTags[name] {
			continue
		}
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
		sig := "<" + name + htmlAttrSuffix(id, class) + ">"
		if id != "" {
			syms = append(syms, Symbol{Name: id, Kind: "html_id", Line: startLine, Signature: truncateSignature(sig)})
		}
		for _, c := range strings.Fields(class) {
			syms = append(syms, Symbol{Name: c, Kind: "html_class", Line: startLine, Signature: truncateSignature(sig)})
		}
	}
	return syms, nil
}

// htmlAttrSuffix renders the id/class of a tag as ` id="..." class="..."` (empty
// when neither is present), mirroring outline.go's htmlIDClass formatting.
func htmlAttrSuffix(id, class string) string {
	out := ""
	if id != "" {
		out += fmt.Sprintf(" id=%q", id)
	}
	if class != "" {
		out += fmt.Sprintf(" class=%q", class)
	}
	return out
}

// truncateSignature caps a signature so one oversized header line can't bloat the
// index or a resolve result.
func truncateSignature(s string) string {
	return stringutil.Truncate(s, maxSignatureLen)
}
