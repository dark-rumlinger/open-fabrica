package content

// Pre-flight validation of a patch hunk ARRAY, before any text is mutated, plus the
// post-apply diagnosis for anchors consumed by an earlier hunk in the same call.
// The pre-flight only rejects defects visible in the array itself (an empty search,
// a hunk with no replace key); everything else is left to the atomic apply, whose
// post-apply classification sees the real outcome instead of predicting it.

import (
	"fmt"
	"strconv"
	"strings"

	"openfabrica/tools"
)

// searchKeyAliases and replaceKeyAliases are the hunk field names weaker models reach for
// out of other editors' patch dialects (Anthropic's Edit tool old_string/new_string, plain
// find/replace, from/to) instead of this tool's search/replace. They are the exact keys the
// empty-search and missing-replace rejects used to name as mistakes; accepting them turns a
// reject-and-retry round trip into a clean apply. The pairs matter together: old_string
// alone would just flip the failure to a missing replace, so both sides alias at once.
var (
	searchKeyAliases  = []string{"old_string", "old_str", "find", "from"}
	replaceKeyAliases = []string{"new_string", "new_str", "new_text", "replacement", "content", "to"}
)

// normalizePatchAliases rewrites alias hunk keys to canonical search/replace IN PLACE,
// before both the struct decode and the pre-flight read the array. A present canonical key
// always wins: an alias only fills a canonical field that is absent (search also fills when
// present-but-empty, since an empty anchor can never match). An explicit replace:"" is a
// deliberate deletion and is left untouched. A genuinely unrecognized key still falls
// through to the empty-search / missing-replace reject.
func normalizePatchAliases(raw []map[string]any) {
	for _, m := range raw {
		if m == nil {
			continue
		}
		// count arrives as a string from some models ("2", "-1"): read it as the number.
		if s, ok := m["count"].(string); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				m["count"] = n
			}
		}
		if s, ok := m["search"].(string); !ok || s == "" {
			for _, k := range searchKeyAliases {
				if v, ok := m[k]; ok {
					m["search"] = v
					delete(m, k)
					break
				}
			}
		}
		if v, ok := m["replace"]; !ok || v == nil {
			for _, k := range replaceKeyAliases {
				if av, ok := m[k]; ok {
					m["replace"] = av
					delete(m, k)
					break
				}
			}
		}
	}
}

// emptySearchHunks returns the 1-based positions of hunks with no search text.
func emptySearchHunks(patches []SearchReplacePatch) []int {
	var out []int
	for i, p := range patches {
		if p.Search == "" && p.Append == "" && p.Prepend == "" {
			out = append(out, i+1)
		}
	}
	return out
}

// missingReplaceHunks returns the 1-based positions of raw hunks that carry a
// search but no replace key at all (a nil value counts as missing). Such a hunk
// decodes to an empty replace and silently deletes the matched span.
func missingReplaceHunks(raw []map[string]any) []int {
	var out []int
	for i, m := range raw {
		s, _ := m["search"].(string)
		if s == "" {
			continue
		}
		if v, ok := m["replace"]; !ok || v == nil {
			out = append(out, i+1)
		}
	}
	return out
}

// patchSetRejection validates the hunk array before any mutation. Returns nil when
// the array is well-formed.
func patchSetRejection(patches []SearchReplacePatch, raw []map[string]any) *tools.Result {
	if empties := emptySearchHunks(patches); len(empties) > 0 {
		return emptySearchResult(empties, len(patches))
	}
	if missing := missingReplaceHunks(raw); len(missing) > 0 {
		return missingReplaceResult(missing, len(patches))
	}
	return nil
}

// emptySearchResult builds the empty-anchor reject. Without it ApplyPatches skips the
// hunk silently, so a batch of nothing but mis-keyed hunks lands as a SUCCESS that
// changed nothing and still bumped the version.
func emptySearchResult(hunks []int, total int) *tools.Result {
	return tools.FailCode("patch_empty_search",
		"patch rejected before any edit was attempted, so the file is UNCHANGED: %s of %d %s an empty `search` and can never match, so it would be silently dropped - a patch that reports success while changing nothing. Each hunk is `{\"search\": \"<exact text from the file>\", \"replace\": \"<new text>\"}` (plus optional `count` and `near`); the common alias keys (`old_string`, `find`, `from`) are accepted automatically, so this hunk carries no anchor under any recognized key. Re-send with the anchor text in `search`.",
		hunkPhrase(hunks), total, hasHave(len(hunks)))
}

// missingReplaceResult builds the missing-replace reject, the mirror of the
// empty-search one: a hunk with no replace key would silently DELETE the span.
func missingReplaceResult(hunks []int, total int) *tools.Result {
	return tools.FailCode("patch_missing_replace",
		"patch rejected before any edit was attempted, so the file is UNCHANGED: %s of %d %s no `replace` key, so the matched span would be silently DELETED. The common alias keys (`new_string`, `new_text`, `replacement`, `content`) are accepted automatically, so this hunk carries no replacement under any recognized key. Re-send with the new text in `replace`; to delete the span on purpose, send `replace: \"\"` explicitly.",
		hunkPhrase(hunks), total, hasHave(len(hunks)))
}

// splitConsumedAnchors partitions unmatched anchors by presence in the ORIGINAL body.
// A consumed anchor was correct when sent - an earlier hunk in the same call rewrote
// that region - while an absent one is real anchor drift.
func splitConsumedAnchors(original string, notFound []string) (consumed, absent []string) {
	for _, s := range notFound {
		if anchorLocatable(original, s) {
			consumed = append(consumed, s)
		} else {
			absent = append(absent, s)
		}
	}
	return consumed, absent
}

// anchorLocatable reports whether search resolves in body under the non-fuzzy layers.
// Fuzzy is excluded: an approximate hit is not evidence enough to claim an earlier hunk
// consumed the region.
func anchorLocatable(body, search string) bool {
	_, _, note, _, ok := locatePatch(body, search, search)
	return ok && !strings.HasPrefix(note, "fuzzy match")
}

// consumedAnchorResult builds the reject for the case where EVERY unmatched anchor is
// present in the original body. The anchors were right, so it steers away from the
// re-read the anchor-drift path prescribes and towards making the hunks independent.
func consumedAnchorResult(kind, path string, consumed, matched []string, total int) *tools.Result {
	return &tools.Result{
		Success: false,
		Code:    "patch_anchor_consumed",
		Error: fmt.Sprintf(
			"%d of %d patch anchors did not match, so NO change was applied and the file is UNCHANGED (patch is atomic - it never half-edits a file). This is NOT anchor drift: do NOT `get` this file and do NOT re-save it. That text IS in the file as you sent it (or within whitespace drift of it) - an EARLIER hunk in this same call already rewrote that region, so by the time this hunk ran the text was gone. Anchors consumed by an earlier hunk: %s. Hunks that DID match are in `matched_rolled_back` - they were undone, not saved. To fix, make the hunks independent: one hunk per region, each `search` covering text no other hunk in the array touches (extend each anchor with the lines around it until the regions do not overlap), and fold two edits to the SAME region into ONE hunk whose `search` spans the whole region and whose `replace` carries both changes. Then re-send the corrected `patches` array in one call.",
			len(consumed), total, strings.Join(quotedLabels(consumed), ", ")),
		Data: map[string]any{
			"kind":                     kind,
			"file_path":                path,
			"matched_rolled_back":      matched,
			"consumed_by_earlier_hunk": shortLabels(consumed),
		},
	}
}

// chainedHunkResult builds the reject for hunks that matched only inside text an
// earlier hunk in the same call had just written. Such a hunk either edits the
// batch's own fresh output or edits one region twice, and applying it throws part of
// the earlier hunk's work away without any error. Consumed and no-op hunks in the
// same batch are named too, so one resend fixes everything.
func chainedHunkResult(kind, path string, pr PatchResult, consumed []string, total int) *tools.Result {
	data := map[string]any{
		"kind":                kind,
		"file_path":           path,
		"matched_rolled_back": pr.Applied,
		"chained":             shortLabels(pr.Chained),
	}
	if len(pr.Warnings) > 0 {
		data["warnings"] = pr.Warnings
	}
	consumedSuffix := ""
	if len(consumed) > 0 {
		data["consumed_by_earlier_hunk"] = shortLabels(consumed)
		consumedSuffix = fmt.Sprintf(" Also, %d anchor(s) were consumed by an earlier hunk in this call (the region was already rewritten before they ran): %s - fold those into the same single hunk per region.",
			len(consumed), strings.Join(quotedLabels(consumed), ", "))
	}
	noopSuffix := ""
	if len(pr.NoOp) > 0 {
		data["no_op"] = shortLabels(pr.NoOp)
		noopSuffix = fmt.Sprintf(" Also, %d edit(s) had `replace` identical to `search` (change nothing): %s - drop them.",
			len(pr.NoOp), strings.Join(quotedLabels(pr.NoOp), ", "))
	}
	return &tools.Result{
		Success: false,
		Code:    "patch_chained_hunk",
		Error: fmt.Sprintf(
			"patch refused, so NO change was applied and the file is UNCHANGED (patch is atomic - it never half-edits a file): %d of %d edit(s) matched only text that an EARLIER hunk in this same call had just written: %s. Applying them would re-edit that hunk's output and silently discard part of it. Hunks apply in order, so give each region exactly ONE hunk: fold every edit to one region into a single hunk whose `replace` carries the whole intended result. If you meant a different occurrence of the same text elsewhere in the file, add `near` with text close to that occurrence. Hunks that DID match are in `matched_rolled_back` - they were undone, not saved. The anchors are correct, so do NOT re-read the file; re-send the corrected `patches` array in one call.%s%s",
			len(pr.Chained), total, strings.Join(quotedLabels(pr.Chained), ", "), consumedSuffix, noopSuffix),
		Data: data,
	}
}

// chainedSuffix names chained hunks inside another reject (a batch whose code is owned
// by a different failure), so the model fixes both in one resend.
func chainedSuffix(data map[string]any, chained []string) string {
	if len(chained) == 0 {
		return ""
	}
	data["chained"] = shortLabels(chained)
	return fmt.Sprintf(" Also, %d edit(s) matched only text an EARLIER hunk in this same call had just written: %s - give each region exactly ONE hunk.",
		len(chained), strings.Join(quotedLabels(chained), ", "))
}

// hunkPhrase renders 1-based hunk positions as "hunk 2" or "hunks 2 and 3".
func hunkPhrase(hunks []int) string {
	if len(hunks) == 1 {
		return fmt.Sprintf("hunk %d", hunks[0])
	}
	parts := make([]string, len(hunks))
	for i, h := range hunks {
		parts[i] = fmt.Sprintf("%d", h)
	}
	return "hunks " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// hasHave picks the verb matching the hunk count.
func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}
