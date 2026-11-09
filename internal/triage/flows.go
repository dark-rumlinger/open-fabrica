package triage

// The user-flow lens: what the replay loop of of_test.js cost, and whether the reds
// it caught were app bugs or script fixes.

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"openfabrica/providers/llm"
)

const (
	// A chase is reported at this many replays, or on a budget refusal, a diagnostic
	// rewrite or a blind app edit.
	flowChaseMinReplays = 6
	// Billed tokens from the first replay convert to score at this multiple of
	// tokensPerCostPoint (they are mostly cache reads).
	flowChaseCacheRateDivisor = 10
	flowChaseDiagnosticWeight = 3
	flowChaseBlindEditWeight  = 1
	// A run is expensive when the share of its billed input after the first replay,
	// and that input, reach these.
	flowChaseExpensiveShare  = 0.5
	flowChaseExpensiveTokens = 1_000_000
)

// flowsFailedCountRe reads the failed-flow count out of a red replay's error.
var flowsFailedCountRe = regexp.MustCompile(`^(\d+) of \d+ flows failed`)

// flowNameRe reads the flow names out of a saved of_test.js body.
var flowNameRe = regexp.MustCompile("\\bname\\s*:\\s*[\"'`]([^\"'`]+)[\"'`]")

// flowChase is what the replay loop of one run cost and what it produced.
type flowChase struct {
	Replays     int // replays that ran (a refusal or skip is not one)
	Exhausted   int // replays refused because the budget was spent or the loop had stalled
	Diagnostic  int // saves that dropped most of the suite while a replay was red
	BlindEdits  int // app edits made after a budget refusal and before the next green replay
	AppFixes    int // reds that went green after the model edited the app
	ScriptFixes int // reds that went green with only the script re-saved
	Tokens      int64
	Share       float64 // Tokens over the run's billed input
	Millis      int64
}

// detectFlowChase tallies the replay loop from the tool dialogue and the call window.
func detectFlowChase(dialogue []dialogueMsg, calls []runCall) flowChase {
	var fc flowChase
	var (
		redOpen       bool // a red replay is waiting for its green
		stalls        int  // replays in a row that got no closer to green
		bestFailed    = -1 // fewest flows failed by a red full run so far
		exhaustedOpen bool
		appWrites     int // successful app edits since the open red began
		prevNames     []string
	)
	for _, m := range dialogue {
		if m.toolCalls == "" {
			continue
		}
		var tcs []llm.ToolCall
		if json.Unmarshal([]byte(m.toolCalls), &tcs) != nil {
			continue
		}
		for _, tc := range tcs {
			result := m.resultByID[tc.ID]
			failed, _, errText := classifyResult(result)
			action := flowToolAction(tc, result)
			if action == "write_flows" || action == "test_flows" {
				if strings.Contains(result, `"budget_exhausted":true`) || strings.Contains(result, `"stalled":true`) {
					fc.Exhausted++
					exhaustedOpen = true
					continue
				}
				replay := action == "test_flows" ||
					flowsFailedRe.MatchString(errText) || len(flowPassed(result)) > 0
				if action == "write_flows" && replay {
					names := flowNamesIn(tc.Arguments)
					if stalls > 0 && droppedMost(prevNames, names) {
						fc.Diagnostic++
					}
					if len(names) > 0 {
						prevNames = names
					}
				}
				if !replay {
					continue
				}
				fc.Replays++
				full := action == "write_flows" || !strings.Contains(tc.Arguments, `"flow"`)
				if failed {
					if !redOpen {
						redOpen, appWrites = true, 0
					}
					// Mirrors the replay budget's stall count.
					n := failedCount(errText)
					switch {
					case !full || n < 0:
						stalls++
					case bestFailed < 0 || n < bestFailed:
						if bestFailed >= 0 {
							stalls = 0
						}
						bestFailed = n
					default:
						stalls++
					}
				} else if len(flowPassed(result)) > 0 {
					if redOpen {
						if appWrites > 0 {
							fc.AppFixes++
						} else {
							fc.ScriptFixes++
						}
					}
					redOpen, exhaustedOpen, stalls = false, false, 0
					if full {
						bestFailed = -1
					}
				}
				continue
			}
			if failed || !isAppWrite(tc) {
				continue
			}
			if redOpen {
				appWrites++
			}
			if exhaustedOpen {
				fc.BlindEdits++
			}
		}
	}
	fc.Tokens, fc.Share, fc.Millis = flowWindow(calls)
	return fc
}

// failedCount reads how many flows a red replay failed from its error, or -1.
func failedCount(errText string) int {
	m := flowsFailedCountRe.FindStringSubmatch(errText)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// flowToolAction is the manage_testing action behind a call, counting a routed
// manage_files edit of the script as a write_flows.
func flowToolAction(tc llm.ToolCall, result string) string {
	if a := testingAction(tc); a != "" {
		return a
	}
	if tc.Name == "manage_files" && strings.Contains(result, `"routed":"of_test.js`) {
		return "write_flows"
	}
	return ""
}

// flowNamesIn lists the flow names a write_flows call saved.
func flowNamesIn(arguments string) []string {
	var a struct {
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(arguments), &a) != nil {
		return nil
	}
	var out []string
	for _, m := range flowNameRe.FindAllStringSubmatch(a.Source, -1) {
		out = append(out, m[1])
	}
	return out
}

// droppedMost reports whether next keeps under half of prev's flows by name.
func droppedMost(prev, next []string) bool {
	if len(prev) < 2 {
		return false
	}
	kept := 0
	for _, p := range prev {
		for _, n := range next {
			if p == n {
				kept++
				break
			}
		}
	}
	return kept*2 < len(prev)
}

// isAppWrite reports whether a tool call changes the app, not just reads, probes or
// saves the script.
func isAppWrite(tc llm.ToolCall) bool {
	switch tc.Name {
	case "manage_testing", "manage_tasks", "manage_guides", "find_tool":
		return false
	}
	action, file := parseToolArgs(tc.Arguments)
	if strings.HasSuffix(file, "of_test.js") {
		return false // the script itself, not the app
	}
	return toolRefKind(toolRef{Name: tc.Name, Action: action}) == "write"
}

// flowWindow measures the calls from the first replay on: billed input, its share of
// the run, and the wall time.
func flowWindow(calls []runCall) (tokens int64, share float64, millis int64) {
	start := -1
	for i, c := range calls {
		for _, r := range c.Calls {
			if r.Name == "manage_testing" && (r.Action == "write_flows" || r.Action == "test_flows") {
				start = i
				break
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return 0, 0, 0
	}
	var total int64
	for i, c := range calls {
		billed := c.Input + c.CacheRead + c.CacheCreate
		total += billed
		if i >= start {
			tokens += billed
		}
	}
	if total > 0 {
		share = float64(tokens) / float64(total)
	}
	first, last := calls[start].At, calls[len(calls)-1].At
	if !first.IsZero() && last.After(first) {
		millis = last.Sub(first).Milliseconds()
	}
	return tokens, share, millis
}

// flowFindings turns a chase into findings and a score. The fix mix is informational.
func flowFindings(fc flowChase) ([]triageFinding, int) {
	var findings []triageFinding
	score := 0
	if fc.Replays >= flowChaseMinReplays || fc.Exhausted > 0 || fc.Diagnostic > 0 || fc.BlindEdits > 0 {
		detail := strconv.Itoa(fc.Replays) + " replays; " + pct(fc.Share) + " of the run's billed input"
		if fc.Millis > 0 {
			detail += " and " + duration(fc.Millis) + " ran after the first replay"
		}
		if fc.Exhausted > 0 {
			detail += "; replay budget refused " + strconv.Itoa(fc.Exhausted) + "x"
		}
		if fc.Diagnostic > 0 {
			detail += "; script rewritten into diagnostics " + strconv.Itoa(fc.Diagnostic) + "x"
		}
		if fc.BlindEdits > 0 {
			detail += "; " + strconv.Itoa(fc.BlindEdits) + " app edits made with no replay left to verify them"
		}
		findings = append(findings, triageFinding{
			Type: "flow_chase", Tool: "manage_testing", Count: fc.Replays, Tokens: fc.Tokens, Detail: detail,
		})
		score += int(fc.Tokens/(tokensPerCostPoint*flowChaseCacheRateDivisor)) +
			fc.Diagnostic*flowChaseDiagnosticWeight + fc.BlindEdits*flowChaseBlindEditWeight
	}
	if fc.AppFixes+fc.ScriptFixes > 0 {
		findings = append(findings, triageFinding{
			Type: "flow_fix_mix", Tool: "manage_testing", Count: fc.AppFixes + fc.ScriptFixes,
			Detail: strconv.Itoa(fc.AppFixes) + " red(s) went green after an app fix (a real bug caught), " +
				strconv.Itoa(fc.ScriptFixes) + " after only a script re-save (not scored)",
		})
	}
	return findings, score
}

// parseCallTime reads an of_llm_calls.created_at value, or the zero time.
func parseCallTime(s string) time.Time {
	t, _ := parseRunTime(s)
	return t
}
