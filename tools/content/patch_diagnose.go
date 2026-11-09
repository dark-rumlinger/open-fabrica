package content

// Extra diagnosis for genuinely absent patch anchors. The anchor-not-found reject
// already hands back the nearest current lines; these checks name the three causes
// that nearest-line matching cannot express, so the model's next call is the right
// fix instead of a re-read:
//   - the hunk's `replace` text is already in the file (the edit already landed),
//   - the anchor text lives in a DIFFERENT artifact (the wrong file was patched),
//   - every anchor line exists, in order, but not contiguously (lines were skipped).

import (
	"fmt"
	"strings"

	"openfabrica/tools"
)

// minDiagnosisLen is the shortest trimmed text a diagnosis will key on. Anything
// shorter (a brace, a bare `return;`) occurs all over a codebase, so a hit carries
// no evidence.
const minDiagnosisLen = 20

// maxOtherArtifacts caps how many other artifacts are named for one anchor.
const maxOtherArtifacts = 3

// missDiagnosis inspects each absent anchor and returns the data fields and message
// suffix to add to the anchor-not-found reject. Each anchor gets at most one
// diagnosis, checked in order of how decisive it is.
func missDiagnosis(ctx *tools.ToolContext, kind, path, body string, absent []string, patches []SearchReplacePatch) (map[string]any, string) {
	replaceFor := map[string]string{}
	for _, p := range patches {
		if _, seen := replaceFor[p.Search]; !seen {
			replaceFor[p.Search] = p.Replace
		}
	}
	var already []string
	var elsewhere []map[string]any
	var gapped []map[string]any
	var b strings.Builder
	for _, s := range absent {
		// Defensive: an already-applied hunk is normally reclassified into
		// AlreadyApplied before `absent` is built, so this rarely fires. Share the one
		// detector so the two paths never disagree.
		if replaceAlreadyPresent(body, s, replaceFor[s]) {
			already = append(already, s)
			continue
		}
		if hits := otherArtifactsContaining(ctx, kind, path, s); len(hits) > 0 {
			elsewhere = append(elsewhere, map[string]any{"anchor": shortLabel(s), "found_in": hits})
			fmt.Fprintf(&b, " The unmatched text %q is not in this file but IS in %s - if that is the file you meant, patch it there instead.",
				shortLabel(s), strings.Join(hits, ", "))
			continue
		}
		if from, to, ok := nonContiguousAnchor(body, s); ok {
			gapped = append(gapped, map[string]any{"anchor": shortLabel(s), "from_line": from, "to_line": to})
			fmt.Fprintf(&b, " Every line of %q exists in this file, in order, between lines %d and %d, but NOT next to each other: the anchor skipped lines. A `search` must be one contiguous run of the file's exact text - copy it verbatim from that range, or anchor on fewer lines.",
				shortLabel(s), from, to)
		}
	}
	data := map[string]any{}
	prefix := ""
	if len(already) > 0 {
		data["already_applied"] = shortLabels(already)
		prefix = fmt.Sprintf(" %d unmatched hunk(s) look ALREADY APPLIED: the file already contains their `replace` text (%s). If so, drop those hunks - there is nothing left to change for them.",
			len(already), strings.Join(quotedLabels(already), ", "))
	}
	if len(elsewhere) > 0 {
		data["found_in_other_artifact"] = elsewhere
	}
	if len(gapped) > 0 {
		data["non_contiguous_anchor"] = gapped
	}
	return data, prefix + b.String()
}

// replaceAlreadyPresent reports whether a hunk whose `search` did not match has in fact
// already been applied: its `replace` (distinct from search, and at least minDiagnosisLen
// of trimmed text so a short common line cannot false-match) is present in body. Fuzzy is
// excluded via anchorLocatable, so an approximate hit never counts. True means the hunk's
// end state already exists, so re-running it changes nothing.
func replaceAlreadyPresent(body, search, replace string) bool {
	if replace == search {
		return false
	}
	if len(strings.TrimSpace(replace)) < minDiagnosisLen {
		return false
	}
	return anchorLocatable(body, replace)
}

// alreadyAppliedSuffix names already-applied hunks inside another reject (one owned by a
// genuine miss), so the model drops them in the same resend instead of re-sending landed
// edits that keep missing.
func alreadyAppliedSuffix(data map[string]any, already []string) string {
	if len(already) == 0 {
		return ""
	}
	data["already_applied"] = shortLabels(already)
	return fmt.Sprintf(" Also, %d edit(s) are ALREADY APPLIED: the file already holds their `replace` text and their `search` is gone, so a previous call landed them: %s. Drop those hunks - there is nothing left to change for them.",
		len(already), strings.Join(quotedLabels(already), ", "))
}

// unanchoredIdenticalResult builds the reject for hunks whose replace equals their
// search while that text is not in the file. Nothing is applied: such a hunk changes
// nothing wherever it lands, and its text usually reads like the result the caller
// wants, so the fix is to move the file's CURRENT text into search. Where a line of
// the hunk's text does exist in the file, it is offered as the anchor to use.
// Other no-op hunks in the batch are named too, so one resend fixes everything.
func unanchoredIdenticalResult(body string, pr PatchResult, total int, stopDirective string, consecutive, cumulative int) *tools.Result {
	data := map[string]any{
		"unanchored_identical": shortLabels(pr.Unanchored),
		"matched_rolled_back":  pr.Applied,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "patch refused, so NO change was applied and the file is UNCHANGED: %d of %d edit(s) have `replace` identical to `search` AND that text is not in the file: %s. An identical hunk changes nothing wherever it lands, so this is NOT an anchor to re-read for: do NOT `get` the file to find it. Text that is not in the file reads like the RESULT you want. `search` must be text the file holds NOW, and `replace` is what that text becomes.",
		len(pr.Unanchored), total, strings.Join(quotedLabels(pr.Unanchored), ", "))
	var anchors []map[string]any
	for _, s := range pr.Unanchored {
		if line := keptLineIn(body, s); line != "" {
			anchors = append(anchors, map[string]any{"hunk": shortLabel(s), "anchor": line})
			fmt.Fprintf(&b, " For %q, the line %q IS in the file: send {\"search\": that line, \"replace\": your full intended text}.", shortLabel(s), line)
		}
	}
	if len(anchors) > 0 {
		data["suggested_anchor"] = anchors
	}
	if len(pr.NoOp) > 0 {
		data["no_op"] = shortLabels(pr.NoOp)
		fmt.Fprintf(&b, " Also, %d edit(s) had `replace` identical to `search` on text that IS in the file (change nothing): %s - drop them.",
			len(pr.NoOp), strings.Join(quotedLabels(pr.NoOp), ", "))
	}
	b.WriteString(insertShapeNote)
	if stopDirective != "" {
		data["noop_loop"] = consecutive
		if cumulative >= noopCumulativeStop {
			data["noop_total"] = cumulative
		}
		b.WriteString(stopDirective)
	}
	return &tools.Result{Success: false, Code: "patch_noop_replace", Error: b.String(), Data: data}
}

// unanchoredSuffix names unanchored identical hunks inside another reject, so the
// model fixes both in one resend.
func unanchoredSuffix(data map[string]any, unanchored []string) string {
	if len(unanchored) == 0 {
		return ""
	}
	data["unanchored_identical"] = shortLabels(unanchored)
	return fmt.Sprintf(" Also, %d edit(s) have `replace` identical to `search` AND that text is not in the file: %s - put the file's CURRENT text in `search` and the text you want in `replace`.",
		len(unanchored), strings.Join(quotedLabels(unanchored), ", "))
}

// keptLineIn returns the last non-trivial line of text that occurs verbatim in body,
// the most likely anchor an insert meant to keep (an insert usually ends on the line
// it goes above). Empty when no line qualifies.
func keptLineIn(body, text string) string {
	lines := strings.Split(normalizeNewlines(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if len(t) >= 8 && strings.Contains(body, t) {
			return t
		}
	}
	return ""
}

// otherArtifactsContaining returns "kind path" labels for up to maxOtherArtifacts
// inline artifacts, other than the one being patched, whose body contains the anchor
// verbatim. Short anchors are skipped: they would match everywhere.
func otherArtifactsContaining(ctx *tools.ToolContext, kind, path, anchor string) []string {
	needle := normalizeNewlines(anchor)
	if ctx == nil || ctx.DB == nil || len(strings.TrimSpace(needle)) < minDiagnosisLen {
		return nil
	}
	rows, err := ctx.DB.Query(`SELECT kind, path FROM of_files
		WHERE is_deleted = 0 AND storage = 'inline' AND NOT (kind = ? AND path = ?) AND instr(body, ?) > 0
		ORDER BY kind, path LIMIT ?`, kind, path, needle, maxOtherArtifacts)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k, p string
		if rows.Scan(&k, &p) == nil {
			out = append(out, fmt.Sprintf("%s %q", k, p))
		}
	}
	if rows.Err() != nil {
		return nil // a hint only: an unreadable list is the same as no other matches
	}
	return out
}

// nonContiguousAnchor reports whether every non-blank line of anchor occurs in body,
// in order (whitespace-trimmed), with other non-blank lines in between, so the anchor
// is the file's text with lines skipped. Returns the 1-based line range the lines
// span. The span is bounded so unrelated lines scattered through a long file do not
// read as a skipped-lines anchor.
func nonContiguousAnchor(body, anchor string) (from, to int, ok bool) {
	var want []string
	for _, ln := range strings.Split(normalizeNewlines(anchor), "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			want = append(want, t)
		}
	}
	if len(want) < 2 {
		return 0, 0, false
	}
	lines := strings.Split(body, "\n")
	trimmed := make([]string, len(lines))
	for i, ln := range lines {
		trimmed[i] = strings.TrimSpace(ln)
	}
	maxSpan := len(want)*3 + 10
	for start := range trimmed {
		if trimmed[start] != want[0] {
			continue
		}
		pos, matched := start, 1
		for matched < len(want) {
			next := -1
			for k := pos + 1; k < len(trimmed) && k-start < maxSpan; k++ {
				if trimmed[k] == want[matched] {
					next = k
					break
				}
			}
			if next < 0 {
				break
			}
			pos, matched = next, matched+1
		}
		if matched < len(want) {
			continue
		}
		nonBlank := 0
		for k := start; k <= pos; k++ {
			if trimmed[k] != "" {
				nonBlank++
			}
		}
		if nonBlank > len(want) {
			return start + 1, pos + 1, true
		}
	}
	return 0, 0, false
}
