package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/internal/stringutil"
)

// The last build's open problems, carried into the first live wake. The validator
// sweep and the checklist are written to the run log and of_tasks at go-live, and the
// owner is told "N checks still failing", but the live agent saw none of it: an owner
// asking "fix the known issues" got an agent that had to rediscover them.

const (
	// maxKnownIssues caps the issues listed, errors first, so one noisy category
	// cannot crowd the seed.
	maxKnownIssues = 12
	// maxKnownIssueChars clips one issue's message; validator prose carries repro
	// detail the agent can re-derive by probing.
	maxKnownIssueChars = 300
)

// knownIssuesBlock renders the latest final validator sweep and the checklist items
// that never got built. Returns "" when the last build shipped clean.
func knownIssuesBlock(r *sql.DB) string {
	issues := lastFinalIssues(r)
	unfinished, _ := unfinishedTasks(r)
	if len(issues) == 0 && len(unfinished) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<known_issues>\nOpen problems the last build shipped with. Some may have been fixed in chat since, so confirm one before working on it.\n")
	if len(issues) > 0 {
		b.WriteString("\nChecks that failed after the build:\n")
		for _, is := range issues {
			where := is.Page
			if where == "" {
				where = is.File
			}
			if where == "" {
				where = "project"
			}
			msg := stringutil.Truncate(strings.TrimSpace(is.Message), maxKnownIssueChars)
			fmt.Fprintf(&b, "- [%s %s] %s: %s\n", is.Severity, is.Category, where, msg)
		}
	}
	if len(unfinished) > 0 {
		b.WriteString("\nPlanned work that was never built:\n")
		for _, t := range unfinished {
			fmt.Fprintf(&b, "- `%s` %s\n", t.ID, t.Title)
		}
	}
	b.WriteString("</known_issues>\n")
	return b.String()
}

// lastFinalIssues reads the newest final validator sweep, errors before warnings,
// capped at maxKnownIssues. Flow findings are left out: they describe a test script
// as much as the app, and the live frame has its own flow rules.
func lastFinalIssues(r *sql.DB) []Issue {
	var payload string
	if err := r.QueryRow(
		`SELECT payload FROM of_run_log WHERE kind = ? ORDER BY id DESC LIMIT 1`,
		RunLogValidation).Scan(&payload); err != nil || payload == "" {
		return nil
	}
	var sweep struct {
		Issues []Issue `json:"issues"`
	}
	if json.Unmarshal([]byte(payload), &sweep) != nil {
		return nil
	}
	var errs, warns []Issue
	for _, is := range sweep.Issues {
		if is.Category == "flow" || is.Category == "explore" {
			continue // a skipped or unrolled check, not a defect to fix
		}
		if is.Severity == "error" {
			errs = append(errs, is)
		} else {
			warns = append(warns, is)
		}
	}
	out := append(errs, warns...)
	if len(out) > maxKnownIssues {
		out = out[:maxKnownIssues]
	}
	return out
}
