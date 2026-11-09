package content

// Insert hunks: the patch forms for adding text without replacing any. The only insert a
// search/replace hunk offers is "repeat the anchor line and add the new lines", and models
// get it wrong in three ways: the replace equals the search (a no-op), the anchor is a
// placeholder that matches nothing, or an invented neighbour line is quoted. These forms
// state the anchor once and the new text once, so none of those can happen.
//
//	{insert_after: "<line>", text: "<new lines>"}   new lines go after the anchor's line
//	{insert_before: "<line>", text: "<new lines>"}  new lines go before the anchor's line
//	{append: "<new lines>"}                         new lines go at the end of the file
//	{prepend: "<new lines>"}                        new lines go at the start of the file
//
// insert_after / insert_before compile to ordinary search/replace hunks before the usual
// matching, so every existing guard (tolerant matching, chained-hunk detection, atomic
// apply) still applies. append / prepend need no anchor, so ApplyPatches applies them
// directly to the body as the earlier hunks left it.

import (
	"fmt"
	"strings"
)

// expandInsertHunks rewrites insert_after / insert_before hunks in place into
// search/replace hunks. It returns an error naming the first hunk it cannot compile (a
// missing text); an anchor the file does not contain exactly once is NOT an error here:
// that is left to the matcher, which reports it with the nearest current lines.
func expandInsertHunks(body string, hunks []map[string]any) error {
	for i, h := range hunks {
		if h == nil {
			continue
		}
		str := func(k string) (string, bool) {
			v, ok := h[k].(string)
			return v, ok
		}
		// An insert hunk's new lines are `text`; `replace` (what the alias pass turns
		// content / new_string into) is accepted for the same slot.
		insertText := func() (string, bool) {
			if t, ok := str("text"); ok {
				return t, true
			}
			return str("replace")
		}
		if after, ok := str("insert_after"); ok {
			text, tok := insertText()
			if !tok || after == "" {
				return fmt.Errorf("hunk %d: insert_after needs a non-empty anchor line and a `text` to insert", i+1)
			}
			search, indent, whole := anchorLines(body, after)
			if whole {
				setHunk(h, search, search+"\n"+indentInsert(text, indent))
			} else {
				setHunk(h, after, strings.TrimRight(after, "\n")+"\n"+strings.TrimRight(text, "\n")+trailingNewline(after))
			}
			continue
		}
		if before, ok := str("insert_before"); ok {
			text, tok := insertText()
			if !tok || before == "" {
				return fmt.Errorf("hunk %d: insert_before needs a non-empty anchor line and a `text` to insert", i+1)
			}
			search, indent, whole := anchorLines(body, before)
			if whole {
				setHunk(h, search, indentInsert(text, indent)+"\n"+search)
			} else {
				setHunk(h, before, strings.TrimRight(text, "\n")+"\n"+before)
			}
			continue
		}
		// append / prepend stay as they are: ApplyPatches handles them directly.
		if _, ok := str("append"); ok {
			delete(h, "text")
		}
		if _, ok := str("prepend"); ok {
			delete(h, "text")
		}
	}
	return nil
}

// anchorLines widens an insert anchor to the whole lines that contain it, so an anchor
// quoted without its indentation, or from the middle of a line, still inserts on its own
// lines. It returns those lines, the first line's leading whitespace, and whether the
// anchor was found verbatim (when not, the matcher's tolerant layers deal with it).
func anchorLines(body, anchor string) (lines, indent string, found bool) {
	a := strings.TrimRight(anchor, "\r\n")
	if a == "" {
		return "", "", false
	}
	idx := strings.Index(body, a)
	if idx < 0 {
		return "", "", false
	}
	start := strings.LastIndex(body[:idx], "\n") + 1
	end := idx + len(a)
	if i := strings.Index(body[end:], "\n"); i >= 0 {
		end += i
	} else {
		end = len(body)
	}
	lines = strings.TrimRight(body[start:end], "\r")
	return lines, leadingWS(lines), true
}

// indentInsert prefixes indent to the inserted lines unless the model already indented
// them, and drops the trailing newline the join adds back.
func indentInsert(text, indent string) string {
	text = strings.TrimRight(text, "\n")
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if leadingWS(ln) != "" {
			return text // already indented by the model
		}
		break
	}
	return reindent(text, indent)
}

// setHunk turns h into a plain search/replace hunk.
func setHunk(h map[string]any, search, replace string) {
	for _, k := range []string{"insert_after", "insert_before", "append", "prepend", "text"} {
		delete(h, k)
	}
	h["search"], h["replace"] = search, replace
}

// trailingNewline is "\n" when s ends with one, so an inserted block keeps the anchor's
// own line ending.
func trailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return "\n"
	}
	return ""
}

// applyEdgeInsert adds p.Append at the end and p.Prepend at the start of body, as the
// earlier hunks left it. It returns the new body and the written spans shifted to match.
func applyEdgeInsert(body string, p SearchReplacePatch, written []span) (string, []span) {
	if p.Append != "" {
		text := strings.TrimRight(p.Append, "\n")
		endsNL := body == "" || strings.HasSuffix(body, "\n")
		base := body
		if base != "" && !endsNL {
			base += "\n"
		}
		start := len(base)
		body = base + text
		if endsNL {
			body += "\n"
		}
		written = append(written, span{start, start + len(text)})
	}
	if p.Prepend != "" {
		text := strings.TrimRight(p.Prepend, "\n") + "\n"
		shifted := make([]span, 0, len(written)+1)
		for _, w := range written {
			shifted = append(shifted, span{w.start + len(text), w.end + len(text)})
		}
		written = append(shifted, span{0, len(text)})
		body = text + body
	}
	return body, written
}
