package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/internal/stringutil"
)

// summaryResult is used by Summarize implementations to parse tool results.
type summaryResult struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	// Code is the machine-readable reject code (e.g. undefined_reference,
	// cyclic_import). Kept through summarization so the compacted chat history and
	// build-log analysis do not lose the one structured field of a failure.
	Code string `json:"code,omitempty"`
}

// ParseSummaryResult parses a JSON tool result string into its components.
// Returns the parsed data map (or array), the error message, and whether parsing succeeded.
func ParseSummaryResult(result string) (r summaryResult, dataMap map[string]any, dataArr []any, ok bool) {
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return r, nil, nil, false
	}
	if !r.Success {
		return r, nil, nil, true
	}
	if arr, isArr := r.Data.([]any); isArr {
		return r, nil, arr, true
	}
	if m, isMap := r.Data.(map[string]any); isMap {
		return r, m, nil, true
	}
	return r, nil, nil, true
}

// SummarizeError returns a standard error summary, preserving the reject code.
// The error text is clipped to a fixed 150-byte prefix with NO ellipsis so the
// result is idempotent under re-summarization (history compaction re-applies this
// to its own output; a growing marker would rewrite a cached mid-history message
// every pass). json.Marshal keeps the output valid even when the error contains
// quotes/backticks. code is dropped from the JSON when empty.
func SummarizeError(errMsg, code string) string {
	if len(errMsg) > 150 {
		errMsg = errMsg[:150]
	}
	b, err := json.Marshal(struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Code    string `json:"code,omitempty"`
	}{Success: false, Error: errMsg, Code: code})
	if err != nil {
		return `{"success":false,"error":"(unserializable error)"}`
	}
	return string(b)
}

// summarizeClip is the ONE truncation length every Summarize hook shares.
// A single figure in every branch is what makes re-summarization idempotent:
// a clipped summary no longer parses as JSON, falls into the unparseable
// branch on re-application, and re-clips to the identical prefix. History
// compaction relies on that - a summary that keeps changing under
// re-application rewrites a cached mid-history message every pass, re-billing
// the whole suffix for nothing. Differing per-hook clip lengths would break this.
const summarizeClip = 300

// SummarizeFrame is the shared shell of a Summarize hook: an unparseable
// result clips, a failure collapses to SummarizeError, a list result collapses
// to the listSummary count line (a fmt string with one %d), and anything else
// runs the tool's detail branch (nil detail, or one returning "", falls back
// to the clip). Idempotent under re-application (see summarizeClip).
func SummarizeFrame(result, listSummary string, detail func(dataMap map[string]any) string) string {
	r, dataMap, dataArr, ok := ParseSummaryResult(result)
	if !ok {
		return stringutil.Truncate(result, summarizeClip)
	}
	if !r.Success {
		return SummarizeError(r.Error, r.Code)
	}
	if dataArr != nil {
		return fmt.Sprintf(`{"success":true,"summary":"`+listSummary+`"}`, len(dataArr))
	}
	if detail != nil {
		if s := detail(dataMap); s != "" {
			return s
		}
	}
	return stringutil.Truncate(result, summarizeClip)
}

// GenericSummarize provides the default summarization for tools that don't
// implement the Summarizer interface.
func GenericSummarize(result string) string {
	return SummarizeFrame(result, "Returned %d items", nil)
}

// SummarizeStripBodies parses the result, keeps any small scalar/identifier
// fields under the data object (path, kind, action, count, etc.), and drops
// fields whose serialised form exceeds bulkyFieldThreshold bytes (artifact
// bodies, rendered DOM, large logs). The kept fields are exactly what the
// model needs after the call has been acted on; the dropped ones are the
// transient bulk that history compaction is trying to evict.
//
// Used as the Summarize for tools whose results contain a mix of small
// metadata + large payloads (manage_files, manage_diagnostics).
func SummarizeStripBodies(result string) string {
	// Already stripped: return it unchanged so re-application is idempotent
	// (the marker key only ever comes from this function's own output).
	if strings.Contains(result, `"_compacted"`) {
		return result
	}
	r, dataMap, dataArr, ok := ParseSummaryResult(result)
	if !ok {
		return stringutil.Truncate(result, 300)
	}
	if !r.Success {
		return SummarizeError(r.Error, r.Code)
	}
	if dataArr != nil {
		return fmt.Sprintf(`{"success":true,"summary":"Returned %d items"}`, len(dataArr))
	}
	if dataMap == nil {
		return stringutil.Truncate(result, 200)
	}
	const bulkyFieldThreshold = 200
	kept := make(map[string]any, len(dataMap))
	dropped := 0
	for k, v := range dataMap {
		raw, err := json.Marshal(v)
		if err != nil || len(raw) > bulkyFieldThreshold {
			dropped++
			continue
		}
		kept[k] = v
	}
	out := map[string]any{
		"success": true,
		"data":    kept,
	}
	if dropped > 0 {
		out["_compacted"] = fmt.Sprintf("dropped %d bulky field(s)", dropped)
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return stringutil.Truncate(result, 300)
	}
	return stringutil.Truncate(string(enc), 600)
}
