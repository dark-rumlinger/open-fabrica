package main

// Summaries and before/after comparison for `openfabrica eval`. Pure over results, so
// they are unit-tested on fixtures.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// evalSummary is the corpus-level view of one eval run.
type evalSummary struct {
	Results []evalResult `json:"results"`
	Totals  evalTotals   `json:"totals"`
}

// evalTotals adds the per-brief numbers up. MetRatio is over every audited item in
// the corpus, not an average of per-brief ratios, so a brief with more items weighs more.
type evalTotals struct {
	Briefs       int     `json:"briefs"`
	Completed    int     `json:"completed"`
	Acceptance   int     `json:"acceptance_failures"`
	Unfinished   int     `json:"unfinished"`
	FeaturesLost int     `json:"features_uncovered"`
	SpecMet      int     `json:"spec_met"`
	SpecAudited  int     `json:"spec_audited"`
	MetRatio     float64 `json:"spec_met_ratio"`
	Errors       int     `json:"errors"`
	OutputTokens int     `json:"output_tokens"`
	BilledInput  int     `json:"billed_input_tokens"`
	DurationSec  float64 `json:"duration_sec"`
}

func summarize(results []evalResult) evalSummary {
	s := evalSummary{Results: results}
	t := &s.Totals
	for _, r := range results {
		t.Briefs++
		if r.Status == "completed" {
			t.Completed++
		}
		t.Acceptance += r.Acceptance
		t.Unfinished += r.Unfinished
		t.FeaturesLost += r.FeaturesLost
		t.SpecMet += r.SpecMet
		t.SpecAudited += r.SpecMet + r.SpecPartial + r.SpecMissing
		for _, n := range r.Errors {
			t.Errors += n
		}
		t.OutputTokens += r.OutputTokens
		t.BilledInput += r.BilledInput
		t.DurationSec += r.DurationSec
	}
	if t.SpecAudited > 0 {
		t.MetRatio = float64(t.SpecMet) / float64(t.SpecAudited)
	}
	return s
}

// evalOneLine is the progress line printed as each brief finishes.
func evalOneLine(r evalResult) string {
	if r.Error != "" && r.RunID == "" {
		return "ERROR " + r.Error
	}
	line := fmt.Sprintf("%s, %d acceptance failures, spec met %d/%d, %d unfinished, %d errors, out %s, billed in %s, %s",
		r.Status, r.Acceptance, r.SpecMet, r.SpecMet+r.SpecPartial+r.SpecMissing, r.Unfinished,
		countSum(r.Errors), kTokens(r.OutputTokens), kTokens(r.BilledInput), minutes(r.DurationSec))
	if r.Error != "" {
		line += " (" + r.Error + ")"
	}
	return line
}

// renderSummary is the corpus table.
func renderSummary(s evalSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s %-10s %6s %8s %6s %6s %9s %10s %7s\n", "brief", "status", "accept", "spec", "unfin", "errors", "out", "billed_in", "time")
	for _, r := range s.Results {
		fmt.Fprintf(&b, "%-24s %-10s %6d %8s %6d %6d %9s %10s %7s\n", clipName(r.Brief), r.Status, r.Acceptance,
			fmt.Sprintf("%d/%d", r.SpecMet, r.SpecMet+r.SpecPartial+r.SpecMissing), r.Unfinished, countSum(r.Errors),
			kTokens(r.OutputTokens), kTokens(r.BilledInput), minutes(r.DurationSec))
	}
	t := s.Totals
	fmt.Fprintf(&b, "%-24s %-10s %6d %8s %6d %6d %9s %10s %7s\n", "TOTAL", fmt.Sprintf("%d/%d", t.Completed, t.Briefs),
		t.Acceptance, fmt.Sprintf("%.0f%%", t.MetRatio*100), t.Unfinished, t.Errors,
		kTokens(t.OutputTokens), kTokens(t.BilledInput), minutes(t.DurationSec))
	return b.String()
}

// cmdEvalCompare prints per-brief and total deltas between two eval runs (B minus A).
func cmdEvalCompare(dirA, dirB string, w io.Writer) int {
	a, err := loadSummary(dirA)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	b, err := loadSummary(dirB)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprint(w, renderCompare(a, b))
	return 0
}

func loadSummary(dir string) (evalSummary, error) {
	var s evalSummary
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s: %w", dir, err)
	}
	return s, nil
}

// renderCompare lines up the briefs both runs share. Lower is better for every
// column except spec met, so a negative delta there is a regression.
func renderCompare(a, b evalSummary) string {
	byBrief := map[string]evalResult{}
	for _, r := range a.Results {
		byBrief[r.Brief] = r
	}
	var names []string
	for _, r := range b.Results {
		if _, ok := byBrief[r.Brief]; ok {
			names = append(names, r.Brief)
		}
	}
	sort.Strings(names)
	bBy := map[string]evalResult{}
	for _, r := range b.Results {
		bBy[r.Brief] = r
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-24s %12s %12s %12s %12s %14s %14s\n", "brief (B - A)", "accept", "spec met", "unfinished", "errors", "out", "billed_in")
	for _, n := range names {
		x, y := byBrief[n], bBy[n]
		fmt.Fprintf(&out, "%-24s %12s %12s %12s %12s %14s %14s\n", clipName(n),
			delta(x.Acceptance, y.Acceptance), delta(x.SpecMet, y.SpecMet), delta(x.Unfinished, y.Unfinished),
			delta(countSum(x.Errors), countSum(y.Errors)), deltaK(x.OutputTokens, y.OutputTokens), deltaK(x.BilledInput, y.BilledInput))
	}
	ta, tb := summarize(pick(a.Results, names)).Totals, summarize(pick(b.Results, names)).Totals
	fmt.Fprintf(&out, "%-24s %12s %12s %12s %12s %14s %14s\n", "TOTAL",
		delta(ta.Acceptance, tb.Acceptance), fmt.Sprintf("%.0f%% -> %.0f%%", ta.MetRatio*100, tb.MetRatio*100),
		delta(ta.Unfinished, tb.Unfinished), delta(ta.Errors, tb.Errors), deltaK(ta.OutputTokens, tb.OutputTokens), deltaK(ta.BilledInput, tb.BilledInput))
	if ta.BilledInput > 0 {
		fmt.Fprintf(&out, "billed input B/A: %.2fx\n", float64(tb.BilledInput)/float64(ta.BilledInput))
	}
	return out.String()
}

func pick(rs []evalResult, names []string) []evalResult {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []evalResult
	for _, r := range rs {
		if want[r.Brief] {
			out = append(out, r)
		}
	}
	return out
}

func countSum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func delta(a, b int) string { return fmt.Sprintf("%d -> %d", a, b) }

func deltaK(a, b int) string { return kTokens(a) + " -> " + kTokens(b) }

func kTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func minutes(sec float64) string { return fmt.Sprintf("%.1fm", sec/60) }

func clipName(s string) string {
	if len(s) > 24 {
		return s[:24]
	}
	return s
}
