package content

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"openfabrica/internal/stringutil"
)

// RegexReplacement represents a single regex replacement operation, applied
// per file by the bulk_replace action (the one regex edit surface left).
type RegexReplacement struct {
	Pattern string `json:"pattern"`
	Replace string `json:"replace"`
	Count   int    `json:"count"` // 0 = replace all, >0 = replace first N
}

// ApplyRegexReplacements applies regex-based replacements to content on
// behalf of bulk_replace. Returns the modified content, number of
// replacements applied, and any error.
//
// The count is MATCHES rewritten, not replacement specs that hit. Counting specs would
// make every per-file count 0 or 1 (bulk_replace always passes exactly one spec) and
// total_applied just a second copy of files_changed, while both the arg description and
// this comment promise replacement counts.
func ApplyRegexReplacements(content string, replacements []RegexReplacement) (string, int, error) {
	modified := content
	totalApplied := 0

	for _, r := range replacements {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return "", 0, fmt.Errorf("invalid regex pattern %q: %w", r.Pattern, err)
		}

		if r.Count <= 0 {
			// Replace all. Count the matches before rewriting: after the rewrite the
			// pattern may no longer match (that is usually the point).
			totalApplied += len(re.FindAllStringIndex(modified, -1))
			modified = re.ReplaceAllString(modified, r.Replace)
		} else {
			// Replace first N occurrences. Expanding against the whole body (not the
			// isolated match) keeps context assertions such as \B, ^ and $ intact.
			locs := re.FindAllStringSubmatchIndex(modified, r.Count)
			var b strings.Builder
			prev := 0
			for _, loc := range locs {
				b.WriteString(modified[prev:loc[0]])
				b.Write(re.ExpandString(nil, r.Replace, modified, loc))
				prev = loc[1]
			}
			b.WriteString(modified[prev:])
			modified = b.String()
			totalApplied += len(locs)
		}
	}

	return modified, totalApplied, nil
}

// SearchReplacePatch represents a single search/replace edit operation.
// Used by pages, layout, and storage patch actions.
type SearchReplacePatch struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
	Count   int    `json:"count,omitempty"` // 0 = first match only (default), -1 = all matches
	Near    string `json:"near,omitempty"`  // context hint to disambiguate which occurrence
	// Append and Prepend add lines at the end / start of the body with no anchor; they are
	// applied to the body as earlier hunks left it.
	Append  string `json:"append,omitempty"`
	Prepend string `json:"prepend,omitempty"`
}

// PatchResult holds the outcome of applying search/replace patches.
type PatchResult struct {
	Applied  []string // short labels of applied patches
	NotFound []string // search strings that weren't found
	NoOp     []string // search strings that matched but whose replace == search (would change nothing)
	Refused  []string // search strings whose edit was refused (fuzzy-located deletion)
	Chained  []string // search strings that matched only inside text an earlier hunk in the same batch wrote
	// AlreadyApplied holds hunks whose `search` is gone from the file but whose
	// `replace` text (distinct from search, and long enough to be unambiguous) is
	// already present. Such a hunk's end state exists, so a PRIOR call already landed
	// it: re-running it is idempotent, not an anchor miss. It never blocks the batch.
	AlreadyApplied []string
	// Unanchored holds hunks whose replace equals their search AND whose text is not
	// in the file. Such a hunk can change nothing wherever it lands, so it is not an
	// anchor to re-read for; its text usually reads like the result the caller wants.
	Unanchored []string
	Warnings   []string // e.g. "matched 3 times, replaced first"
}

// span is a half-open byte range [start, end) in the body being patched.
type span struct{ start, end int }

// ApplyPatches applies search/replace patches to content using a LAYERED
// matcher: exact -> line-ending-normalized -> trailing-whitespace-insensitive
// -> indentation-flexible -> fuzzy. A model-authored anchor that drifts from
// the stored body by a CR, a trailing space, or a uniform indent shift still
// lands instead of silently returning not-found and triggering a re-read loop.
// By default replaces only the FIRST occurrence to prevent accidental
// duplication. Set count=-1 for replace-all, or count=N for first N occurrences.
//
// Hunks apply in order, each against the result of the previous one. A hunk whose
// match lies entirely inside text an earlier hunk of the same batch wrote is not
// applied: it lands in Chained instead. That match is either an edit to text the
// batch itself just produced or a second edit to a region an earlier hunk already
// rewrote, and applying it silently discards part of the earlier hunk's work.
func ApplyPatches(content string, patches []SearchReplacePatch) (string, PatchResult) {
	modified := content
	var result PatchResult
	// Byte ranges of `modified` written by hunks applied earlier in this batch.
	var written []span

	for _, p := range patches {
		if p.Append != "" || p.Prepend != "" {
			modified, written = applyEdgeInsert(modified, p, written)
			result.Applied = append(result.Applied, shortLabel(strings.TrimSpace(p.Append+p.Prepend)))
			continue
		}
		if p.Search == "" {
			continue
		}

		// Identical search and replace changes nothing wherever it lands, so it is
		// never an anchor miss to re-read for. It splits by whether the text is in
		// the file, because the two need different fixes:
		//   - text IS present (under the same tolerant layers the locate step
		//     uses) -> a pure no-op: the anchor is correct. NoOp bucket; the
		//     caller surfaces it (and rejects only when the WHOLE batch changed
		//     nothing - a no-op bundled with real hunks must not block them,
		//     that turned one slip into a retry cascade).
		//   - text is NOT present -> Unanchored. The hunk usually carries the
		//     result the caller wants in both fields, so the fix is to put the
		//     file's current text in search, not to re-read and re-anchor.
		if p.Search == p.Replace {
			if anchorLocatable(modified, p.Search) {
				result.NoOp = append(result.NoOp, p.Search)
			} else {
				result.Unanchored = append(result.Unanchored, p.Search)
			}
			continue
		}

		// Layered locate: find the exact substring in `modified` that this
		// patch's search corresponds to, tolerating line-ending / whitespace /
		// indentation drift, with a fuzzy last resort. actualSearch is always a
		// real substring of `modified`, so the replacement below is byte-exact.
		actualSearch, replaceText, note, at, ok := locatePatch(modified, p.Search, p.Replace)
		if !ok {
			// Idempotent hunk: the anchor is gone but the file already holds this hunk's
			// `replace` end state, so a previous call applied it. Classify it apart from a
			// genuine miss - it must not block the atomic batch and roll back the sibling
			// hunks that still change bytes. Checked against the ORIGINAL body (`content`),
			// so a replace an earlier hunk in THIS batch produced is not mistaken for one a
			// prior call landed.
			if replaceAlreadyPresent(content, p.Search, p.Replace) {
				result.AlreadyApplied = append(result.AlreadyApplied, p.Search)
			} else {
				result.NotFound = append(result.NotFound, p.Search)
			}
			continue
		}

		// Content-loss guard: deleting a span (an empty/whitespace replacement)
		// that was located only by the fuzzy last-resort matcher is the shape that
		// silently truncates a file - the matcher is unsure where it landed and the
		// edit removes content rather than changing it. Refused into its own bucket
		// (not NotFound: the text WAS located, so a not-found claim would be false).
		if strings.TrimSpace(replaceText) == "" && strings.HasPrefix(note, "fuzzy match") {
			result.Refused = append(result.Refused, p.Search)
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"patch refused: %q would DELETE a span located only by an approximate (fuzzy) match - the pattern that silently truncates a file. Re-send with an exact `search` copied verbatim from the file, or use `save` to rewrite it.",
				shortLabel(p.Search)))
			continue
		}

		occ := occurrenceStarts(modified, actualSearch)
		if at >= 0 {
			// A line-window layer knows exactly where it landed; the window's text may
			// also occur earlier as a mid-line substring, which is not the place meant.
			occ = []int{at}
		}
		occurrences := len(occ)
		label := shortLabel(p.Search)

		// Pick the occurrence(s) to replace: the one nearest `near` when it is set and
		// the anchor repeats, otherwise the first N (default 1, count:-1 for all).
		var targets []int
		nearMissed := false
		if p.Near != "" && occurrences > 1 {
			idx, found := nearestOccurrence(modified, occ, len(actualSearch), p.Near)
			targets, nearMissed = []int{idx}, !found
		} else {
			n := 1
			if p.Count == -1 || p.Count > occurrences {
				n = occurrences
			} else if p.Count > 0 {
				n = p.Count
			}
			targets = occ[:n]
		}

		if withinWritten(targets, len(actualSearch), written) {
			result.Chained = append(result.Chained, p.Search)
			continue
		}
		if actualSearch != replaceText {
			modified, written = spliceAt(modified, targets, len(actualSearch), replaceText, written)
		}

		result.Applied = append(result.Applied, label)

		if note != "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%q applied via tolerant match (%s) - confirm the result is what you intended.", label, note))
		}
		switch {
		case nearMissed:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%q matched %d times and its near text %q is not in the file, so the FIRST match was replaced. Check it is the one you meant.", label, occurrences, shortLabel(p.Near)))
		case occurrences > 1 && p.Near == "" && len(targets) < occurrences:
			which := "first only"
			if len(targets) > 1 {
				which = fmt.Sprintf("the first %d", len(targets))
			}
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%q matched %d times, replaced %s. Use count:-1 for all, or add near:\"context\" to target a specific one.", label, occurrences, which))
		}
	}

	return modified, result
}

// occurrenceStarts returns the start offset of every non-overlapping occurrence of
// sub in s, left to right (the same occurrences strings.Replace rewrites).
func occurrenceStarts(s, sub string) []int {
	var out []int
	for off := 0; off <= len(s); {
		i := strings.Index(s[off:], sub)
		if i < 0 {
			break
		}
		out = append(out, off+i)
		off += i + len(sub)
	}
	return out
}

// nearestOccurrence picks the occurrence whose center is closest to the center of
// the first occurrence of near. When near is not in s it falls back to the first
// occurrence and reports found=false.
func nearestOccurrence(s string, occ []int, n int, near string) (idx int, found bool) {
	nearIdx := strings.Index(s, near)
	if nearIdx == -1 {
		return occ[0], false
	}
	nearCenter := nearIdx + len(near)/2
	best, bestDist := occ[0], -1
	for _, o := range occ {
		dist := o + n/2 - nearCenter
		if dist < 0 {
			dist = -dist
		}
		if bestDist < 0 || dist < bestDist {
			best, bestDist = o, dist
		}
	}
	return best, true
}

// withinWritten reports whether any target span [t, t+n) lies entirely inside one
// of the written ranges. A span that only shares a boundary with written text, or
// extends past it into untouched text, does not count.
func withinWritten(targets []int, n int, written []span) bool {
	for _, t := range targets {
		for _, w := range written {
			if w.start <= t && t+n <= w.end {
				return true
			}
		}
	}
	return false
}

// spliceAt replaces the n-byte span at each target (ascending, non-overlapping) with
// repl, and returns the new body plus the written ranges shifted into its
// coordinates with one range added per replacement.
func spliceAt(s string, targets []int, n int, repl string, written []span) (string, []span) {
	delta := len(repl) - n
	var b strings.Builder
	b.Grow(len(s) + delta*len(targets))
	prev := 0
	added := make([]span, 0, len(targets))
	for i, t := range targets {
		b.WriteString(s[prev:t])
		start := t + i*delta
		b.WriteString(repl)
		added = append(added, span{start, start + len(repl)})
		prev = t + n
	}
	b.WriteString(s[prev:])
	// Map each earlier range into the new body. An endpoint that falls strictly
	// inside a replaced span snaps to that replacement's edge, so a range the new
	// edit partly overwrote grows to cover the replacement instead of pointing at
	// shifted, unrelated bytes.
	mapPos := func(pos int, isEnd bool) int {
		k := 0
		for i, t := range targets {
			if t < pos && pos < t+n {
				if isEnd {
					return t + i*delta + len(repl)
				}
				return t + i*delta
			}
			if t+n <= pos {
				k++
			}
		}
		return pos + k*delta
	}
	out := make([]span, 0, len(written)+len(added))
	for _, w := range written {
		out = append(out, span{mapPos(w.start, false), mapPos(w.end, true)})
	}
	return b.String(), append(out, added...)
}

// shortLabel truncates a search string for human-readable result labels.
func shortLabel(s string) string {
	return stringutil.Truncate(s, 60)
}

// fuzzyOpsBudget caps the line-based fuzzy layer by its true cost. similarity is
// Levenshtein, O(target*window) ~ O(target^2) per window, run over ~len(fileLines)
// windows, so the work is ~len(fileLines)*len(target)^2 rune-ops. Gating on that
// (rather than the old len(fileLines)*len(target), which under-priced a long anchor
// and over-priced a short one) lets the common large-file + short-anchor case keep
// its tolerant last resort - where a hard not-found used to force a re-read round-trip -
// while still skipping the genuinely expensive large-file + long-anchor case. ~50M
// rune-ops is the ceiling (tens of ms); past it the model re-anchors instead.
const fuzzyOpsBudget = 50_000_000

// locatePatch finds the exact substring of `modified` that `search` refers to,
// trying progressively looser matchers, and returns that substring plus the
// (possibly re-indented / newline-normalized) replacement text and a note
// naming the layer that matched. The returned actualSearch is always a real
// substring of `modified`, so callers can replace it byte-exactly.
//
// at is the byte offset of the located span when the matcher knows exactly where
// it landed (the line-window layers), else -1: the caller then picks among the
// literal occurrences of actualSearch. Re-finding a window's text by substring
// search would land on an earlier mid-line hit and edit the wrong place.
func locatePatch(modified, search, replace string) (actualSearch, replaceText, note string, at int, ok bool) {
	// Only re-introduce CRLF into the replacement if the body already uses it;
	// otherwise keep the body LF-clean.
	repl := replace
	if !strings.Contains(modified, "\r") {
		repl = normalizeNewlines(replace)
	}

	// Layer 1: exact.
	if strings.Contains(modified, search) {
		return search, repl, "", -1, true
	}

	// Layer 1b: literal escape sequences. A model sometimes types the search with
	// literal two-character escapes - backslash-n, backslash-t, backslash-r - where
	// the file has real newlines/tabs, so the raw search never matches a multi-line
	// region. If unescaping yields a match, use it. The replacement is unescaped only
	// when it carries no real newline of its own and no escape that sits inside a
	// quoted string: a `.split('\n')` written into code must stay two characters.
	if unesc := unescapeLiterals(search); unesc != search {
		uRepl := repl
		if !strings.ContainsAny(replace, "\n\r") && !escapeInsideQuotes(replace) {
			uRepl = unescapeLiterals(replace)
			if !strings.Contains(modified, "\r") {
				uRepl = normalizeNewlines(uRepl)
			}
		}
		const note = `interpreted literal \n / \t / \r escapes in the search as real newlines/tabs`
		if strings.Contains(modified, unesc) {
			return unesc, uRepl, note, -1, true
		}
		if uLF := normalizeNewlines(unesc); uLF != unesc && strings.Contains(modified, uLF) {
			return uLF, uRepl, note, -1, true
		}
	}

	// Layer 2: line-ending normalization. The model commonly sends CRLF in the
	// search while the stored body is LF - the "patch with \r didn't match" case.
	if sLF := normalizeNewlines(search); sLF != search && strings.Contains(modified, sLF) {
		return sLF, repl, "line endings normalized (CRLF->LF)", -1, true
	}

	// Layers 3-5 work line-by-line. Skip an all-whitespace search (no anchor).
	searchLF := normalizeNewlines(search)
	searchLines := strings.Split(strings.TrimSuffix(searchLF, "\n"), "\n")
	if allBlank(searchLines) {
		return "", "", "", -1, false
	}
	fileLines := strings.Split(modified, "\n")
	n := len(searchLines)
	if n > len(fileLines) {
		return "", "", "", -1, false
	}
	// The layers below match whole lines and drop the search's trailing newline, so
	// a located span ends before the file's line break. When the search itself ended
	// with a newline, take the file's line break into the span too, as an exact match
	// would have; otherwise a replace that also ends with a newline adds a blank line.
	endsNL := strings.HasSuffix(searchLF, "\n")
	// window returns the span of lines [i, i+n) and its byte offset in modified.
	window := func(i int) (string, int) {
		start := 0
		for k := 0; k < i; k++ {
			start += len(fileLines[k]) + 1
		}
		found := strings.Join(fileLines[i:i+n], "\n")
		if endsNL && !strings.HasSuffix(found, "\n") && strings.HasPrefix(modified[start+len(found):], "\n") {
			found += "\n"
		}
		return found, start
	}

	// Layer 3: trailing-whitespace-insensitive (also absorbs lone CR in the body).
	for i := 0; i+n <= len(fileLines); i++ {
		if blockEqual(fileLines[i:i+n], searchLines, trimRightWS) {
			found, start := window(i)
			return found, repl, "ignoring trailing whitespace", start, true
		}
	}

	// Layer 4: indentation-flexible. Lines match ignoring leading whitespace and
	// the file carries a uniform extra indent; re-indent the replacement to fit.
	for i := 0; i+n <= len(fileLines); i++ {
		if prefix, okk := indentFlexMatch(fileLines[i:i+n], searchLines); okk {
			found, start := window(i)
			return found, reindent(repl, prefix), "indentation adjusted to fit", start, true
		}
	}

	// Layer 4b: whitespace-collapsed, unique. Layers 3-5 all compare an n-line
	// window against an n-line search, so an anchor whose LINE COUNT differs from
	// the file can never match them - the model writing `.toast-bubble { position:
	// fixed;` on one line where the file has the brace and the property on two.
	// Collapsing every whitespace run to one space makes the two identical, and
	// requiring the collapsed form to occur EXACTLY ONCE is what keeps that safe:
	// with a second candidate there is no way to know which the model meant, so it
	// falls through to fuzzy rather than guessing.
	if found, start, okk := collapsedUniqueMatch(modified, search); okk {
		if endsNL && !strings.HasSuffix(found, "\n") && strings.HasPrefix(modified[start+len(found):], "\n") {
			found += "\n"
		}
		return found, repl, "whitespace normalized (line breaks / indentation differ)", start, true
	}

	// Layer 5: fuzzy. Best-scoring n-line window by normalized similarity,
	// accepted only when clearly above threshold and ahead of the runner-up.
	target := normalizeBlock(searchLines)
	if len(target) == 0 || len(fileLines)*len(target)*len(target) > fuzzyOpsBudget {
		return "", "", "", -1, false
	}
	best, bestScore, second := -1, 0.0, 0.0
	for i := 0; i+n <= len(fileLines); i++ {
		score := similarity(normalizeBlock(fileLines[i:i+n]), target)
		switch {
		case score > bestScore:
			second, bestScore, best = bestScore, score, i
		case score > second:
			second = score
		}
	}
	if best >= 0 && bestScore >= 0.90 && bestScore-second >= 0.05 {
		found, start := window(best)
		return found, repl,
			fmt.Sprintf("fuzzy match ~%.0f%% - VERIFY this is the right place", bestScore*100), start, true
	}
	return "", "", "", -1, false
}

// escapeInsideQuotes reports whether s holds a literal \n, \t or \r two-character
// escape inside a quoted string, the shape of real code that must keep the escape.
func escapeInsideQuotes(s string) bool {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			if quote != 0 && (s[i+1] == 'n' || s[i+1] == 't' || s[i+1] == 'r') {
				return true
			}
			i++
		case quote == 0 && (c == '\'' || c == '"' || c == '`'):
			quote = c
		case quote != 0 && c == quote:
			quote = 0
		}
	}
	return false
}

// unescapeLiterals turns the literal two-character escapes a model commonly
// emits in a patch search (\n \t \r) into the bytes they denote, so a search
// typed with escapes still matches a body with real newlines/tabs. Only these
// three are handled; any other backslash sequence is left untouched.
func unescapeLiterals(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r").Replace(s)
}

// normalizeNewlines collapses CRLF and lone CR to LF.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func allBlank(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

func trimRightWS(s string) string { return strings.TrimRight(s, " \t\r") }

// blockEqual reports whether two equal-length line blocks match under norm.
func blockEqual(a, b []string, norm func(string) string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if norm(a[i]) != norm(b[i]) {
			return false
		}
	}
	return true
}

// leadingWS returns the leading spaces/tabs of s.
func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// indentFlexMatch reports whether fileBlock equals searchBlock ignoring leading
// whitespace, with a single uniform extra indent `prefix` that fileBlock carries
// over searchBlock on every non-blank line. Returns that prefix so the
// replacement can be re-indented to match the file.
func indentFlexMatch(fileBlock, searchBlock []string) (string, bool) {
	if len(fileBlock) != len(searchBlock) {
		return "", false
	}
	prefix, have := "", false
	for i := range fileBlock {
		fTrim, sTrim := strings.TrimSpace(fileBlock[i]), strings.TrimSpace(searchBlock[i])
		if fTrim != sTrim {
			return "", false
		}
		if fTrim == "" {
			continue // blank line: indentation irrelevant
		}
		fLead, sLead := leadingWS(fileBlock[i]), leadingWS(searchBlock[i])
		if !strings.HasPrefix(fLead, sLead) {
			return "", false // less-indented or mixed ws: not a clean uniform shift
		}
		p := fLead[len(sLead):]
		if !have {
			prefix, have = p, true
		} else if p != prefix {
			return "", false // non-uniform shift
		}
	}
	if !have || prefix == "" {
		return "", false // all blank, or zero delta (exact/trailing-ws would have caught it)
	}
	return prefix, true
}

// reindent prepends prefix to every non-blank line of s.
func reindent(s, prefix string) string {
	if prefix == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

// normalizeBlock joins lines with interior whitespace collapsed, for similarity
// scoring that ignores indentation and trailing-space noise.
func normalizeBlock(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(out, "\n")
}

// similarity returns 1 - levenshtein(a,b)/max(runeLen) in [0,1]. levenshtein counts
// rune edits, so the denominator is a rune count too: dividing by the byte length would
// inflate the score on multi-byte text and make the fuzzy layer accept a weaker match.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if la == 0 || lb == 0 {
		return 0
	}
	m := la
	if lb > m {
		m = lb
	}
	return 1 - float64(levenshtein(a, b))/float64(m)
}

// nearestRegions returns, for each unmatched patch anchor, the region of the
// CURRENT body whose text most resembles the anchor's first meaningful line,
// framed with 1-based line numbers. On a patch-anchor miss the anchor is
// genuinely absent (the matcher already tolerates line-ending/whitespace/indent
// drift and a fuzzy last resort), so the model's next move is to re-anchor from
// the real file - otherwise a separate `get` round-trip. Handing back the
// closest current lines here collapses that get -> patch retry into a single
// patch: the model copies a verbatim anchor straight from this snippet. Returns
// nil when nothing is close enough, so a truly unrelated anchor yields no noise.
func nearestRegions(body string, notFound []string) []map[string]any {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 {
		return nil
	}
	const context = 3
	seen := map[int]bool{}
	var out []map[string]any
	for _, anchor := range notFound {
		probe := firstMeaningfulLine(anchor)
		if probe == "" {
			continue
		}
		bestIdx, bestScore := -1, 1<<30
		for i, ln := range lines {
			t := strings.TrimSpace(ln)
			if t == "" {
				continue
			}
			d := levenshtein(probe, t)
			if d < bestScore {
				bestScore, bestIdx = d, i
			}
		}
		// Require a real resemblance: the closest line must overlap the probe by
		// more than half its length, else there is no useful "did you mean here".
		if bestIdx < 0 || bestScore > len(probe)/2+2 || seen[bestIdx] {
			continue
		}
		seen[bestIdx] = true
		start := bestIdx - context
		if start < 0 {
			start = 0
		}
		end := bestIdx + context
		if end >= len(lines) {
			end = len(lines) - 1
		}
		var b strings.Builder
		for i := start; i <= end; i++ {
			fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
		}
		out = append(out, map[string]any{
			"anchor":         shortLabel(anchor),
			"nearest_line":   bestIdx + 1,
			"current_region": b.String(),
		})
	}
	if len(out) == 0 && len(notFound) > 0 {
		// Nothing in the file resembles the anchor, so the model quoted text that was never
		// there. Give it the shape of the file to re-anchor from instead of a bare miss.
		out = append(out, map[string]any{
			"anchor":      shortLabel(notFound[0]),
			"file_lines":  len(lines),
			"first_lines": numberedLines(lines, 0, 20),
			"last_lines":  numberedLines(lines, len(lines)-20, len(lines)),
			"note":        "no line of the file resembles this anchor, so it was not copied from this file: anchor on a line from first_lines / last_lines, or get the region you mean",
		})
	}
	return out
}

// numberedLines renders lines[from:to] (clamped) as "N: text" rows.
func numberedLines(lines []string, from, to int) string {
	if from < 0 {
		from = 0
	}
	if to > len(lines) {
		to = len(lines)
	}
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
	}
	return b.String()
}

// firstMeaningfulLine returns the first non-blank, trimmed line of s, the stable
// probe for locating where an anchor was meant to land.
func firstMeaningfulLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// levenshtein computes edit distance with two rolling rows.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			best := prev[j] + 1
			if ins := curr[j-1] + 1; ins < best {
				best = ins
			}
			if sub := prev[j-1] + cost; sub < best {
				best = sub
			}
			curr[j] = best
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// collapseWS returns s with every run of whitespace collapsed to a single space
// and leading/trailing whitespace dropped, plus a map from each byte of the
// collapsed form back to its offset in s. The map is what lets a match found in
// collapsed space be reported as an exact substring of the original.
func collapseWS(s string) (string, []int) {
	var b strings.Builder
	offsets := make([]int, 0, len(s))
	inWS, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			inWS = true
			continue
		}
		if inWS && started {
			b.WriteByte(' ')
			offsets = append(offsets, i)
		}
		inWS = false
		started = true
		b.WriteByte(c)
		offsets = append(offsets, i)
	}
	return b.String(), offsets
}

// collapsedUniqueMatch finds search in content ignoring how whitespace is
// distributed, and returns the exact substring of content it corresponds to.
//
// It reports a match ONLY when the collapsed search occurs exactly once. That
// uniqueness is the whole safety argument: the layers above this one are
// anchored to real structure (same lines, same trailing space, a uniform indent
// shift), whereas this one deliberately throws away the line structure the model
// wrote, so a second candidate means the anchor no longer identifies a place.
// A blank or whitespace-only search never matches.
func collapsedUniqueMatch(content, search string) (string, int, bool) {
	cs, _ := collapseWS(search)
	if cs == "" {
		return "", -1, false
	}
	cc, offsets := collapseWS(content)
	first := strings.Index(cc, cs)
	if first < 0 {
		return "", -1, false
	}
	if strings.Contains(cc[first+1:], cs) {
		return "", -1, false // ambiguous: let fuzzy decide, with its own margin check
	}
	// offsets[i] is where collapsed byte i started in content; the span ends at
	// the last matched byte, whose original width is one byte (collapsed bytes are
	// either a literal content byte or an inserted separator space).
	start := offsets[first]
	end := offsets[first+len(cs)-1] + 1
	if start < 0 || end > len(content) || start >= end {
		return "", -1, false
	}
	return content[start:end], start, true
}
