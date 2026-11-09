package tools

import (
	"strings"
	"testing"
)

// The summarizers must be idempotent: history compaction may visit a message
// it already summarized (a fresh compactor after a task boundary, an edge path
// without the index memo), and a summary that changes under re-application
// rewrites a cached mid-history message, re-billing every byte after it.
func TestSummarizersIdempotent(t *testing.T) {
	inputs := []string{
		`{"success":true,"data":{"design":"` + strings.Repeat("d", 500) + `","title":"x"}}`,
		`{"success":true,"data":[1,2,3,4,5]}`,
		`{"success":false,"error":"` + strings.Repeat("e", 300) + `"}`,
		`{"success":false,"error":"write rejected: ` + strings.Repeat("long ", 60) + `","code":"undefined_reference"}`,
		strings.Repeat("not json at all ", 50),
	}
	for _, in := range inputs {
		once := GenericSummarize(in)
		if twice := GenericSummarize(once); twice != once {
			t.Errorf("GenericSummarize not idempotent:\n once  %q\n twice %q", once, twice)
		}
		onceStrip := SummarizeStripBodies(in)
		if twiceStrip := SummarizeStripBodies(onceStrip); twiceStrip != onceStrip {
			t.Errorf("SummarizeStripBodies not idempotent:\n once  %q\n twice %q", onceStrip, twiceStrip)
		}
	}
}

// A failed result's reject code must survive summarization - it is the one
// structured field build-log triage groups on, and every summarizer the chat
// history runs through can drop it. Also proves the JSON stays valid when the
// error text carries quotes/backticks (json.Marshal, not string interpolation).
func TestSummarizeError_PreservesCode(t *testing.T) {
	in := "{\"success\":false,\"error\":\"write rejected: `getHooks` is not defined - add \\\"import\\\"\",\"code\":\"undefined_reference\"}"
	for _, got := range []string{GenericSummarize(in), SummarizeStripBodies(in), SummarizeError("boom", "cyclic_import")} {
		r, _, _, ok := ParseSummaryResult(got)
		if !ok {
			t.Fatalf("summary is not valid JSON: %q", got)
		}
		if r.Success {
			t.Errorf("expected failure summary, got success: %q", got)
		}
	}
	if got := GenericSummarize(in); !strings.Contains(got, `"code":"undefined_reference"`) {
		t.Errorf("GenericSummarize dropped the code: %q", got)
	}
	if got := SummarizeStripBodies(in); !strings.Contains(got, `"code":"undefined_reference"`) {
		t.Errorf("SummarizeStripBodies dropped the code: %q", got)
	}
}
