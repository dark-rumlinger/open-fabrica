package triage

// The text rendering of a scan. The JSON shape is what tooling reads; this is
// what a person (or an agent triaging a build) reads after a run, and it exists
// so answering "what went wrong and what did it cost" needs no second query.

import (
	"sort"
	"strconv"
	"strings"
)

// findingOrder ranks finding types worst-first within a build, so the line that
// explains the verdict is the line at the top. Types absent here sort last.
var findingOrder = map[string]int{
	"build_failed":    0,
	"stage_failure":   1,
	"validator_issue": 2,
	"probe_failure":   3,
	"patch_loop":      4,
	"flow_chase":      5,
	"cache_thrash":    6,
	"cold_miss":       7,
	"redundant_save":  8,
	"reread":          9,
	"batch_tasks":     10,
	"task_ordering":   11,
	"llm_error":       12,
	"tool_reject":     13,
	"flow_fix_mix":    14,
	"self_correction": 15,
}

// RenderText renders a scan as a plain-text report, worst build first.
func RenderText(rep Report) string {
	var b strings.Builder
	b.WriteString("triage " + rep.GeneratedAt + "  projects=" + strconv.Itoa(rep.ProjectsScanned) +
		"  builds=" + strconv.Itoa(rep.BuildsScanned) + "  flagged=" + strconv.Itoa(rep.BuildsFlagged) + "\n")
	if len(rep.Builds) == 0 {
		b.WriteString("\nno builds matched\n")
		return b.String()
	}
	for _, tb := range rep.Builds {
		b.WriteString("\n" + strings.Repeat("-", 72) + "\n")
		renderBuildText(&b, tb)
	}
	return b.String()
}

func renderBuildText(b *strings.Builder, tb triageBuild) {
	b.WriteString(tb.Build + "  " + tb.ProjectName + "  (project " + strconv.Itoa(tb.Project) + ")")
	if tb.Model != "" {
		b.WriteString("  " + tb.Model)
	}
	b.WriteString("\nstatus=" + tb.Status)
	if tb.Stage != "" {
		b.WriteString(" stage=" + tb.Stage)
	}
	if tb.DurationMS > 0 {
		b.WriteString("  " + duration(tb.DurationMS))
	}
	b.WriteString("  VERDICT: " + strings.ToUpper(tb.Verdict) +
		" (score " + strconv.Itoa(tb.Score) + ")\n")

	c := tb.Cost
	if c.Calls > 0 {
		b.WriteString("\nCOST   billed_in=" + tokens(c.BilledInputTokens) +
			"  out=" + tokens(c.OutputTokens) +
			"  cache_hit=" + pct(c.CacheHitRatio) +
			"  calls=" + strconv.Itoa(c.Calls))
		if c.OverheadRatio > 0 {
			b.WriteString("  overhead=" + strconv.FormatFloat(c.OverheadRatio, 'f', 1, 64) + "x")
		}
		b.WriteString("\n")
		if c.RebilledTokens > 0 || c.ColdMissTokens > 0 {
			b.WriteString("       re-bought " + tokens(c.RebilledTokens+c.ColdMissTokens) +
				" of context that caching should have covered\n")
		}
		if c.FlowTokens > 0 {
			b.WriteString("       " + pct(c.FlowShare) + " of billed input (" + tokens(c.FlowTokens) +
				") came after the first user-flow replay\n")
		}
	}

	b.WriteString("\nFINDINGS\n")
	if len(tb.Findings) == 0 {
		b.WriteString("  none\n")
	}
	ranked := append([]triageFinding(nil), tb.Findings...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return findingRank(ranked[i].Type) < findingRank(ranked[j].Type)
	})
	for _, f := range ranked {
		line := "  " + pad(f.Type, 15)
		if f.Count > 0 {
			line += pad("x"+strconv.Itoa(f.Count), 5)
		} else {
			line += pad("", 5)
		}
		line += f.Detail
		if f.Tool != "" && !strings.Contains(f.Detail, f.Tool) {
			line += " [" + f.Tool + "]"
		}
		if f.Code != "" {
			line += " (" + f.Code + ")"
		}
		if f.Tokens > 0 {
			line += " ~" + tokens(f.Tokens)
		}
		b.WriteString(line + "\n")
	}

	if len(tb.events) > 0 {
		b.WriteString("\nTIMELINE (calls that re-bought context)\n")
		for _, e := range tb.events {
			line := "  it" + pad(strconv.Itoa(e.Iteration), 4) + pad(e.Stage, 7) +
				"fresh_in=" + pad(tokens(e.Input), 8) +
				"cache_read=" + pad(tokens(e.CacheRead), 8)
			switch e.Kind {
			case "cold_miss":
				line += "COLD MISS"
			case "cache_thrash":
				line += "PREFIX -" + kb(e.PrefixDrop)
			}
			b.WriteString(line + "\n")
		}
	}
}

func findingRank(t string) int {
	if r, ok := findingOrder[t]; ok {
		return r
	}
	return len(findingOrder)
}

// tokens renders a token count compactly (1501455 -> 1.50M).
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 2, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "K"
	default:
		return strconv.FormatInt(n, 10)
	}
}

func pct(f float64) string {
	return strconv.Itoa(int(f*100+0.5)) + "%"
}

// duration renders a run's wall time (735000 -> 12m15s).
func duration(ms int64) string {
	s := ms / 1000
	if s < 60 {
		return strconv.FormatInt(s, 10) + "s"
	}
	return strconv.FormatInt(s/60, 10) + "m" + strconv.FormatInt(s%60, 10) + "s"
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len(s))
}
