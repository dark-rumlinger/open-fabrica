package probe

// A static cross-check of a flow script against the project's own files. A flow is
// usually written long after the pages it drives, from memory, so a selector or a label
// it names may not exist anywhere. Finding that out from a browser replay costs a run and
// reads as "no element matches"; finding it here costs nothing and says why.

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

var (
	refQuotedRe = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	refParenRe  = regexp.MustCompile(`\([^)]*\)`)
	refIDRe     = regexp.MustCompile(`#([A-Za-z_][\w-]*)`)
	refClassRe  = regexp.MustCompile(`\.([A-Za-z_][\w-]*)`)
	refAttrRe   = regexp.MustCompile(`\[\s*([A-Za-z_][\w-]*)`)
	refUnicode  = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)
	refTmplRe   = regexp.MustCompile(`\{\{[^}]*\}\}`)
)

// maxRefWarnings bounds how many unmatched references one save reports.
const maxRefWarnings = 8

// minRefTextLen is the shortest visible-text literal worth checking: shorter ones
// ("OK", "Go") appear everywhere and prove nothing.
const minRefTextLen = 4

// UnmatchedReferences returns advisory warnings for the ids, classes, attribute names and
// visible-text literals the flows name that appear in none of corpus (the project's page,
// layout, script and stylesheet bodies concatenated). Advisory only: a class built from
// pieces at runtime, or text rendered from data the app loads, is legitimately absent
// from the files.
func (s *FlowSuite) UnmatchedReferences(corpus string) []string {
	if strings.TrimSpace(corpus) == "" {
		return nil
	}
	text := strings.ToLower(refUnicode.ReplaceAllStringFunc(html.UnescapeString(corpus), unescapeU))
	text = strings.Join(strings.Fields(text), " ")
	var out []string
	seen := map[string]bool{}
	report := func(flow string, step int, st FlowStep, kind, tok string) {
		key := kind + "\x00" + tok
		if seen[key] || len(out) >= maxRefWarnings {
			return
		}
		seen[key] = true
		out = append(out, fmt.Sprintf("flow %q step %d %s uses %s %q, which appears in no page, script or stylesheet of this project. "+
			"Read the file that renders it and copy the real selector or label (a name built at runtime, or text loaded from data, is fine).",
			flow, step, st, kind, tok))
	}
	for _, f := range s.Flows {
		// Values filled or typed by the flow itself legitimately appear on screen
		// even when absent from static project source files.
		flowText := text
		var typed strings.Builder
		for _, st := range f.Steps {
			if st.Value != nil && *st.Value != "" && (st.Fill != "" || st.Type != "" || st.Select != "") {
				typed.WriteByte(' ')
				typed.WriteString(strings.ToLower(strings.Join(strings.Fields(*st.Value), " ")))
			}
		}
		if typed.Len() > 0 {
			flowText += typed.String()
		}
		for i, st := range f.Steps {
			for _, sel := range stepSelectors(st) {
				if lit, ok := strings.CutPrefix(sel, "text="); ok {
					checkText(flowText, lit, func(tok string) { report(f.Name, i+1, st, "the label", tok) })
					if !strings.Contains(lit, "{{") {
						norm := strings.ToLower(strings.Join(strings.Fields(lit), " "))
						if len(norm) >= minRefTextLen && strings.Contains(flowText, norm) && !hasExactLabelOccurrence(flowText, norm) {
							key := "exact-label\x00" + lit
							if !seen[key] && len(out) < maxRefWarnings {
								seen[key] = true
								out = append(out, fmt.Sprintf("flow %q step %d %s uses %q, which requires an exact element label match, but %q only appears inside longer text in this project. "+
									"Copy the full button/link label or use :has-text(%q) for a substring match.",
									f.Name, i+1, st, sel, lit, lit))
							}
						}
					}
					continue
				}
				checkSelector(corpus, sel, func(kind, tok string) { report(f.Name, i+1, st, kind, tok) })
			}
			if st.ExpectText != "" {
				checkText(flowText, st.ExpectText, func(tok string) { report(f.Name, i+1, st, "the text", tok) })
			}
		}
	}
	return out
}

// stepSelectors lists the CSS selectors a step targets, including its in: scope.
func stepSelectors(st FlowStep) []string {
	var sels []string
	for _, v := range []string{
		st.Click, st.DblClick, st.Hover, st.Drag, st.Fill, st.Type, st.Select, st.Check, st.Uncheck,
		st.Upload, st.Download, st.WaitFor, st.ExpectVisible, st.ExpectHidden, st.ExpectCount,
		st.Snapshot, st.ExpectChanged, st.To, st.In, st.Scroll,
	} {
		if v == "" || v == "top" || v == "bottom" {
			continue
		}
		sels = append(sels, v)
	}
	if st.Remember != "" && st.From != "" && !strings.HasPrefix(st.From, "response.") && !strings.HasPrefix(st.From, "email.") {
		sels = append(sels, st.From)
	}
	return sels
}

func unescapeU(m string) string {
	var r rune
	if _, err := fmt.Sscanf(m[2:], "%x", &r); err != nil {
		return m
	}
	return string(r)
}

// checkText reports lit when it is long enough to mean something, is not a template and is
// absent from text (already lowercased, entity-decoded and whitespace-collapsed).
func checkText(text, lit string, report func(string)) {
	if strings.Contains(lit, "{{") {
		return
	}
	norm := strings.ToLower(strings.Join(strings.Fields(lit), " "))
	if len(norm) < minRefTextLen || strings.Contains(text, norm) {
		return
	}
	report(lit)
}

// hasExactLabelOccurrence reports whether norm appears in text bounded like a
// standalone UI label (>label<, "label", 'label', or `label`), rather than only
// as a substring inside a longer phrase.
func hasExactLabelOccurrence(text, norm string) bool {
	start := 0
	for {
		idx := strings.Index(text[start:], norm)
		if idx < 0 {
			return false
		}
		abs := start + idx
		leftOK := abs == 0
		if !leftOK {
			i := abs - 1
			if text[i] == ' ' && i > 0 {
				i--
			}
			switch text[i] {
			case '>', '"', '\'', '`':
				leftOK = true
			}
		}
		end := abs + len(norm)
		rightOK := end == len(text)
		if !rightOK {
			i := end
			if text[i] == ' ' && i+1 < len(text) {
				i++
			}
			switch text[i] {
			case '<', '"', '\'', '`':
				rightOK = true
			}
		}
		if leftOK && rightOK {
			return true
		}
		start = abs + 1
	}
}

// checkSelector reports each id, class and attribute name in sel that appears nowhere in
// corpus. Quoted values, pseudo-class arguments and {{templates}} are ignored.
func checkSelector(corpus, sel string, report func(kind, tok string)) {
	sel = refTmplRe.ReplaceAllString(sel, "")
	sel = refQuotedRe.ReplaceAllString(sel, "")
	sel = refParenRe.ReplaceAllString(sel, "")
	for _, m := range refIDRe.FindAllStringSubmatch(sel, -1) {
		if len(m[1]) >= 3 && !strings.Contains(corpus, m[1]) {
			report("the id", "#"+m[1])
		}
	}
	for _, m := range refClassRe.FindAllStringSubmatch(sel, -1) {
		if len(m[1]) >= 3 && !strings.Contains(corpus, m[1]) {
			report("the class", "."+m[1])
		}
	}
	for _, m := range refAttrRe.FindAllStringSubmatch(sel, -1) {
		if !attrInCorpus(corpus, m[1]) {
			report("the attribute", m[1])
		}
	}
}

// attrInCorpus reports whether an attribute name appears in corpus, either literally or,
// for data-*, as the camelCase key a script reads through dataset.
func attrInCorpus(corpus, name string) bool {
	if strings.Contains(corpus, name) {
		return true
	}
	rest, ok := strings.CutPrefix(name, "data-")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	key := strings.Join(parts, "")
	return strings.Contains(corpus, "dataset."+key) || strings.Contains(corpus, "dataset['"+key) || strings.Contains(corpus, `dataset["`+key)
}
