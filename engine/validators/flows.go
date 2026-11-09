package validators

// The user-flow validator: runs the journeys the build wrote to of_test.js
// (signup, login, create an item, ...) through headless Chrome and reports the
// first failing step of each flow. Complements runtime_probe, which only LOADS
// pages: a handler that throws on submit, or a list that never shows the item just
// added, is invisible until something clicks. A project with pages or an API but
// no of_test.js is itself an error: a headless API's journeys are request steps.
// Self-skips (as a warning) when Chrome is not installed.

import (
	"context"
	"database/sql"

	"openfabrica/internal/flowsnap"
	"openfabrica/probe"
	"openfabrica/store"
)

type flowTestsValidator struct{}

func (flowTestsValidator) Name() string { return "flow_tests" }

// Sequential: the replay writes through the app under a data snapshot and restores it,
// which must not land while another validator is reading the same project.
func (flowTestsValidator) Sequential() {}

func (flowTestsValidator) Validate(ctx context.Context, pdb *sql.DB, baseURL, hostHeader string) []Issue {
	src := probe.LoadFlowSource(pdb)
	if src == "" {
		if !hasPages(pdb) {
			if !hasAPI(pdb) {
				return nil // nothing a person or a client could use yet
			}
			return []Issue{{
				Severity:      store.SeverityError,
				Category:      "flow",
				File:          probe.FlowPath,
				Deterministic: true,
				Signature:     "flow|missing",
				Message: "this project serves an API but has no of_test.js, so nothing verifies a client can actually use it. " +
					"Write its journeys with manage_testing action=write_flows as request steps - create, read back, update and delete each resource, " +
					"sign up and log in, a refused request (bad input, no permission) with its status - chaining ids with remember from response.<field>, " +
					"then fix what the replay write_flows runs as it saves reveals.",
			}}
		}
		return []Issue{{
			Severity:      store.SeverityError,
			Category:      "flow",
			File:          probe.FlowPath,
			Deterministic: true,
			Signature:     "flow|missing",
			Message: "this project has pages but no of_test.js, so nothing verifies that a person can actually use it. " +
				"Write its user journeys with manage_testing action=write_flows - whatever a real user does here: sign up and log in, " +
				"create/edit/delete each kind of data, play the game (keys, clicks, expect the canvas to change or the loop to run), " +
				"draw, drag, upload, chat as two users, browse every page of a site - then fix what the replay write_flows runs as it saves reveals.",
		}}
	}
	suite, err := probe.ParseFlowSource(src)
	if err != nil {
		// write_flows rejects an invalid script at save time, so this only fires when
		// the file reached the store another way; still a real, fixable defect.
		return []Issue{{
			Severity:      store.SeverityError,
			Category:      "flow",
			File:          probe.FlowPath,
			Deterministic: true,
			Signature:     "flow|parse",
			Message:       err.Error() + " Re-save it with manage_testing action=write_flows.",
		}}
	}

	// Replay between a snapshot of the app's data and its restore, so the flows'
	// orders, uploads and status changes are rolled back afterwards. Outside the
	// engine (no snapshot opener) only test accounts can be cleaned up.
	var res probe.FlowResult
	var issues []Issue
	if flowsnap.OpenerFrom(ctx) != nil {
		skip, restoreErr := flowsnap.Around(ctx, func() {
			res = probe.RunFlowTests(ctx, suite, baseURL, hostHeader, "")
		})
		if skip != "" {
			res = probe.FlowResult{Skipped: skip}
		}
		if restoreErr != nil {
			issues = append(issues, Issue{
				Severity:  store.SeverityWarning,
				Category:  "flow",
				File:      probe.FlowPath,
				Signature: "flow|rollback",
				Message:   "the user-flow replay's data changes could not be fully rolled back: " + restoreErr.Error(),
			})
		}
	} else {
		res = probe.RunFlowTests(ctx, suite, baseURL, hostHeader, "")
		// Remove the accounts the run registered, whatever the outcome.
		probe.CleanupFlowUsers(probe.FlowWriteDBFromContext(ctx), res.Accounts)
	}

	for _, i := range res.Issues {
		if i.Severity == store.SeverityError {
			issues = append(issues, i)
		}
	}
	if res.Skipped != "" {
		issues = append(issues, Issue{
			Severity:  store.SeverityWarning,
			Category:  "flow",
			File:      probe.FlowPath,
			Signature: probe.FlowSkippedSignaturePrefix + probe.SkipKind(res.Skipped),
			Message:   "user-flow tests in of_test.js were skipped: " + res.Skipped + " (the journeys were not verified).",
		})
	}
	return issues
}

// apiTables are the tables whose rows are endpoints a client calls.
var apiTables = []string{"of_api_endpoints", "of_function_endpoints", "of_auth_endpoints", "of_upload_endpoints",
	"of_llm_endpoints", "of_agent_endpoints", "of_query_endpoints", "of_feed_endpoints", "of_endpoints"}

// hasAPI reports whether the project serves any API endpoint. A table that does
// not exist (the project never used that kind) counts as none.
func hasAPI(pdb *sql.DB) bool {
	for _, t := range apiTables {
		var n int
		if pdb.QueryRow(`SELECT COUNT(*) FROM `+t).Scan(&n) == nil && n > 0 {
			return true
		}
	}
	return false
}

// hasPages reports whether the project serves any page. Every project with a UI
// - app, game, tool, or static site - owes an of_test.js, and so does one with an
// API; only a project with neither does not.
func hasPages(pdb *sql.DB) bool {
	var n int
	err := pdb.QueryRow(`SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0`).Scan(&n)
	return err == nil && n > 0
}

func init() {
	RegisterValidator(flowTestsValidator{})
}
