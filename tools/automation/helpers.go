package automation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/cron"
	"openfabrica/internal/sqlguard"
	"openfabrica/security"
	"openfabrica/tools"
)

// Shared infrastructure for the automation tools: jobs-table bootstrapping,
// native-action validation, cron next-fire calculation, payload templating,
// and the trigger result summarizer. Tool-facing schemas live in the
// manage_*.go files; this file is the implementation surface they share.

// validateNativeAction parses a native_action JSON payload and enforces the
// shape the native-action dispatcher can handle. Catching bad values here
// keeps the worker from logging "unknown native action type" at run time.
func validateNativeAction(raw string) error {
	if raw == "" {
		return nil
	}
	var a struct {
		Type         string `json:"type"`
		Table        string `json:"table"`
		Column       string `json:"column"`
		Age          string `json:"age"`
		SQL          string `json:"sql"`
		EventType    string `json:"event_type"`
		EndpointPath string `json:"endpoint_path"`
	}
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if a.Type == "" {
		return fmt.Errorf("type is required (one of: delete_stale, count_rows, truncate, sql, trigger_event, run_function)")
	}
	switch a.Type {
	case "delete_stale":
		if a.Table == "" || a.Age == "" {
			return fmt.Errorf("delete_stale requires table and age")
		}
		if !isSafeIdent(a.Table) || (a.Column != "" && !isSafeIdent(a.Column)) {
			return fmt.Errorf("delete_stale: table/column must be alphanumeric or underscore")
		}
	case "count_rows", "truncate":
		if a.Table == "" {
			return fmt.Errorf("%s requires table", a.Type)
		}
		if !isSafeIdent(a.Table) {
			return fmt.Errorf("%s: table must be alphanumeric or underscore", a.Type)
		}
	case "sql":
		if _, err := sqlguard.CheckAction(a.SQL, "SELECT", "INSERT", "UPDATE", "DELETE"); err != nil {
			return fmt.Errorf("sql action: %w", err)
		}
	case "trigger_event":
		if a.EventType == "" {
			return fmt.Errorf("trigger_event requires event_type")
		}
	case "run_function":
		if a.EndpointPath == "" {
			return fmt.Errorf("run_function requires endpoint_path (the manage_function endpoint to invoke)")
		}
	default:
		return fmt.Errorf("unknown type %q (expected one of: delete_stale, count_rows, truncate, sql, trigger_event, run_function)", a.Type)
	}
	return nil
}

// isSafeIdent is the platform's identifier rule (security.IdentifierRegex), the one
// a table or column had to pass to be created, so a name accepted here exists.
func isSafeIdent(name string) bool { return security.IdentifierRegex.MatchString(name) }

// minLLMScheduleInterval is the floor cadence for a prompt-backed (LLM)
// schedule. Each fire of a prompt schedule wakes the FULL agent (a real LLM
// session with the whole toolset), so a high-frequency one drains tokens fast
// and spams the owner's chat. Deterministic upkeep that must run often belongs
// in a native_action (no LLM, no floor). One hour is the floor because periodic
// *reasoning* work (digests, reminders, summaries) is hourly or rarer; anything
// more frequent is almost always a heartbeat that should be a native_action or
// not exist at all (e.g. an every-minute "is the data still persisted?" check).
const minLLMScheduleInterval = time.Hour

// scheduleCadence returns the SMALLEST gap between consecutive fires of a cron-
// or interval-driven schedule. An interval is its own gap; a cron is sampled
// across its next several fires and the minimum gap taken, so an irregular
// expression (e.g. "0 9 * * 1-5", whose Fri->Mon gap dwarfs its Mon->Tue gap)
// is judged by its tightest cadence, not an average. ok is false when neither a
// cron nor a positive interval is set, or the cron never fires.
//
// The cron wins when both are set, exactly as the scheduler advances next_run
// (engine/scheduled.go): judging the interval instead let a prompt schedule with an
// hourly interval and a "* * * * *" cron pass the floor and fire every minute.
func scheduleCadence(cronExpr string, intervalSec int, now time.Time) (gap time.Duration, ok bool) {
	if cronExpr == "" {
		if intervalSec > 0 {
			return time.Duration(intervalSec) * time.Second, true
		}
		return 0, false
	}
	prev, err := cron.NextTime(cronExpr, now)
	if err != nil {
		return 0, false
	}
	const samples = 64
	min := time.Duration(1<<63 - 1)
	for i := 0; i < samples; i++ {
		next, err := cron.NextTime(cronExpr, prev)
		if err != nil {
			break
		}
		if d := next.Sub(prev); d > 0 && d < min {
			min = d
		}
		prev = next
	}
	if min == time.Duration(1<<63-1) {
		return 0, false
	}
	return min, true
}

// guardLLMCadence returns a non-empty rejection reason when a prompt-backed
// schedule (one with a prompt and no native_action) would fire more often than
// minLLMScheduleInterval. A native_action schedule is exempt: it runs without
// the LLM, so frequent firing is cheap. The reason is written for the build
// agent, steering it to slow the schedule down or convert it to a native_action.
func guardLLMCadence(prompt, nativeAction, cronExpr string, intervalSec int, now time.Time) string {
	if strings.TrimSpace(nativeAction) != "" || strings.TrimSpace(prompt) == "" {
		return "" // native (no LLM) or nothing LLM-backed to guard
	}
	gap, ok := scheduleCadence(cronExpr, intervalSec, now)
	if !ok || gap >= minLLMScheduleInterval {
		return ""
	}
	return fmt.Sprintf(
		"a prompt schedule wakes the full agent on every run (a real LLM call), so it must run at most once per hour - this one fires about every %s. Raise the interval to hourly or slower, OR, if it does deterministic upkeep (counting, pruning, persisting rows), use a native_action (type: count_rows | delete_stale | sql | truncate | trigger_event | run_function) which runs WITHOUT the LLM and has no cadence floor. If it is a heartbeat/health-check (e.g. \"confirm rows are still persisted\"), drop it entirely: rows persist on write, no periodic ping is needed.",
		humanizeCadence(gap))
}

// humanizeCadence renders a gap as a short owner/agent-readable cadence.
func humanizeCadence(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d second(s)", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s)", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d hour(s)", int(d.Hours()))
	}
}

// triggerSummarize is the Summarize hook for manage_triggers: collapses
// list results to a count line and individual operations to "Action 'X'
// processed" so history compaction sheds the verbose row data.
func triggerSummarize(result string) string {
	return tools.SummarizeFrame(result, "Listed %d actions", func(dataMap map[string]any) string {
		if name, _ := dataMap["name"].(string); name != "" {
			return fmt.Sprintf(`{"success":true,"summary":"Action '%s' processed"}`, name)
		}
		return ""
	})
}
