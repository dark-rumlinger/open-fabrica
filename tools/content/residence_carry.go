package content

import "openfabrica/tools"

// Read residence across an edit. A patch changes the body's fingerprint, and the read
// tracker used to drop everything it knew at the old one, so the next `get` re-sent the
// whole file although the model held all of it but the few lines it just changed (and
// the echo showed those). carryResidence maps every line the model held before the edit
// onto its line in the edited body, through the same line diff the echo uses; the
// caller then adds the echoed spans. When the two together cover the new body, it is
// recorded as held in full.

// carriedLines returns the new-body line numbers of unchanged lines the model held
// before the edit (full, or within ranges), as merged [from,to] spans.
func carriedLines(oldBody, newBody string, full bool, ranges [][2]int) [][2]int {
	held := func(line int) bool {
		if full {
			return true
		}
		for _, r := range ranges {
			if r[0] <= line && line <= r[1] {
				return true
			}
		}
		return false
	}
	var spans [][2]int
	aLine, bLine := 1, 1
	for _, op := range lcsDiff(splitLinesKeep(oldBody), splitLinesKeep(newBody)) {
		switch op.kind {
		case diffEq:
			if held(aLine) {
				if n := len(spans); n > 0 && spans[n-1][1] == bLine-1 {
					spans[n-1][1] = bLine
				} else {
					spans = append(spans, [2]int{bLine, bLine})
				}
			}
			aLine++
			bLine++
		case diffIns:
			bLine++
		case diffDel:
			aLine++
		}
	}
	return spans
}

// carryResidence records, at the edited body's fingerprint, the lines the model still
// holds from before the edit, plus the echoed spans; full when they cover every line.
func carryResidence(ctx *tools.ToolContext, kind, path, oldBody, newBody string, echoed [][2]int) {
	if ctx.ReadTracker == nil || oldBody == newBody {
		return
	}
	key := tools.ReadCacheKey(kind, path)
	full, ranges := ctx.ReadTracker.Delivered(key, tools.ReadFingerprint(oldBody))
	if !full && len(ranges) == 0 {
		return
	}
	spans := append(carriedLines(oldBody, newBody, full, ranges), echoed...)
	fp := tools.ReadFingerprint(newBody)
	if coversLines(spans, len(splitLinesKeep(newBody))) {
		ctx.ReadTracker.RecordDelivered(key, fp)
		return
	}
	for _, sp := range spans {
		ctx.ReadTracker.RecordDeliveredRange(key, fp, sp[0], sp[1])
	}
}

// coversLines reports whether the spans together cover lines 1..n.
func coversLines(spans [][2]int, n int) bool {
	if n == 0 {
		return true
	}
	covered := make([]bool, n+1)
	for _, sp := range spans {
		for l := sp[0]; l <= sp[1] && l <= n; l++ {
			if l >= 1 {
				covered[l] = true
			}
		}
	}
	for l := 1; l <= n; l++ {
		if !covered[l] {
			return false
		}
	}
	return true
}
