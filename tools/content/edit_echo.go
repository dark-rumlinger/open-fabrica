package content

import (
	"fmt"
	"strings"
)

// Post-edit echo for the mutating manage_files actions (patch and friends).
// After an edit, the model otherwise has no current view of the file and would
// re-`get` it just to re-orient. Echoing the changed region with post-edit
// line numbers makes each edit self-orienting and kills the
// get->edit->get->edit re-scroll loop.

// changedLineRanges returns the 1-indexed line ranges in newBody that differ
// from oldBody, coalescing strictly-adjacent touched lines into a single range.
// A deletion is reported as the (now-current) line where the removed text sat,
// so the echo shows the surrounding context. Returns nil when the bodies are
// byte-identical.
func changedLineRanges(oldBody, newBody string) [][2]int {
	if oldBody == newBody {
		return nil
	}
	ops := lcsDiff(splitLinesKeep(oldBody), splitLinesKeep(newBody))

	var touched []int // 1-indexed new-body line numbers, in ascending order
	bLine := 1
	for _, op := range ops {
		switch op.kind {
		case diffEq:
			bLine++
		case diffIns:
			touched = append(touched, bLine)
			bLine++
		case diffDel:
			// The deleted line is absent from newBody; mark the line now at this
			// position (bLine) so the echo shows where the removal happened.
			touched = append(touched, bLine)
		}
	}
	if len(touched) == 0 {
		return nil
	}

	// Coalesce adjacent / duplicate touched lines into ranges.
	var ranges [][2]int
	start, end := touched[0], touched[0]
	for _, ln := range touched[1:] {
		if ln <= end+1 {
			if ln > end {
				end = ln
			}
			continue
		}
		ranges = append(ranges, [2]int{start, end})
		start, end = ln, ln
	}
	ranges = append(ranges, [2]int{start, end})
	return ranges
}

// echoChangedRegion renders the changed ranges of newBody with current
// (post-edit) line numbers, each range expanded by `context` lines and clamped
// to the file bounds. Overlapping expanded ranges are merged; multiple hunks are
// separated by an elided divider. The whole block is capped at `budget` bytes
// via trimToByteBudget. Returns the rendered text, whether it was truncated, and
// the [from,to] line spans actually rendered in full (a span dropped by budget
// truncation is omitted, so a caller can record exactly the lines the model
// received). An empty ranges slice (no change) returns ("", false, nil).
func echoChangedRegion(newBody string, ranges [][2]int, context, budget int) (string, bool, [][2]int) {
	if len(ranges) == 0 {
		return "", false, nil
	}
	if context < 0 {
		context = 0
	}
	lines := splitLinesKeep(newBody)
	total := len(lines)
	if total == 0 {
		return "", false, nil
	}

	// Expand by context and clamp to [1, total].
	type span struct{ start, end int }
	var spans []span
	for _, r := range ranges {
		s := r[0] - context
		e := r[1] + context
		if s < 1 {
			s = 1
		}
		if e > total {
			e = total
		}
		if s > total {
			s = total
		}
		spans = append(spans, span{s, e})
	}

	// Merge overlapping / adjacent expanded spans (ranges arrive sorted).
	merged := spans[:0:0]
	cur := spans[0]
	for _, sp := range spans[1:] {
		if sp.start <= cur.end+1 {
			if sp.end > cur.end {
				cur.end = sp.end
			}
			continue
		}
		merged = append(merged, cur)
		cur = sp
	}
	merged = append(merged, cur)

	var b strings.Builder
	// endOffset[i] is the byte length of the rendered block up to and including
	// span i, so after budget-trimming we can tell which spans survived whole.
	endOffset := make([]int, len(merged))
	for i, sp := range merged {
		if i > 0 {
			fmt.Fprintf(&b, "\n  ... (lines %d-%d) ...\n", merged[i-1].end+1, sp.start-1)
		}
		window := strings.Join(lines[sp.start-1:sp.end], "\n")
		b.WriteString(numberLines(window, sp.start))
		endOffset[i] = b.Len()
	}

	out := b.String()
	trimmed, _ := trimToByteBudget(out, budget)
	truncated := len(trimmed) < len(out)

	// Report only spans whose full rendered text fits within the trimmed output,
	// so a caller never records residence over a hunk the model didn't receive.
	var shownSpans [][2]int
	for i, sp := range merged {
		if endOffset[i] > len(trimmed) {
			break
		}
		shownSpans = append(shownSpans, [2]int{sp.start, sp.end})
	}
	return trimmed, truncated, shownSpans
}
