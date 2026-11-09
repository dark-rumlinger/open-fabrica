package engine

import (
	"sort"
	"strconv"
	"strings"

	"openfabrica/db/models"
	"openfabrica/store"
)

// The go-live gate: the single home for "which findings must never ship".
//
// A finding blocks a deploy when all three hold: its category is page-fatal
// (blockingCategories), it is error-severity, and it is Deterministic (a flake
// can never block). Policy is category-driven, not a per-issue flag: "may this
// ship" is deployment policy, not a property of the finding.

// blockingCategories are the categories whose error-severity findings leave the
// app broken for a real user (browser-observable): an uncaught throw or failed
// asset, a broken websocket, a module that won't parse or bind, an import that
// can't resolve, a page whose every control is dead, or one whose every control
// is invisible (hidden_ui - the page opens with nothing to click at all).
//
// Not here: undefined_reference and arity_mismatch (call-position inferences
// whose path may never run), api_target (speculative), css_syntax (cosmetic),
// dead_control (page still has a working control), the readiness/conformance
// validators (ship a usable app), and rls_scope (a real leak, but blocking on it
// is a separate policy call). These drive a repair round, never a deploy block.
var blockingCategories = map[string]bool{
	"runtime":          true,
	"websocket":        true,
	"js_syntax":        true,
	"js_binding":       true,
	"module_imports":   true,
	"module_specifier": true,
	"inert_page":       true,
	"hidden_ui":        true,
}

// isBlocking reports whether a single finding must stop a deploy.
func isBlocking(is Issue) bool {
	return is.Severity == store.SeverityError && is.Deterministic && blockingCategories[is.Category]
}

// blockingErrors returns the findings that must stop a deploy, ordered stably by
// fingerprint so the owner's message and the log read the same way across runs.
func blockingErrors(issues []Issue) []Issue {
	out := make([]Issue, 0, len(issues))
	for _, is := range issues {
		if isBlocking(is) {
			out = append(out, is)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Fingerprint() < out[j].Fingerprint() })
	return out
}

// Deploy policies, read from projects.config (models.ProjectDeployPolicy). An
// unset or unrecognized value means defaultDeployPolicy, so the default lives in
// Go and config stores only what the owner actually chose.
const (
	policyShadow = "shadow" // report what WOULD block, then deploy anyway
	policyStrict = "strict" // a blocking finding stops the deploy
	policyForce  = "force"  // owner override: ship regardless
)

// defaultDeployPolicy is shadow: a page-fatal finding is logged as would_block and
// the build still deploys. That is the standing position, not a staging step. A
// refused deploy leaves the owner with nothing to look at, while the fix loop and
// the known-issues report already carry the signal, so a hard gate is opt-in: a
// project that wants one sets strict in projects.config.
const defaultDeployPolicy = policyShadow

// deployPolicy returns the effective policy for this project, falling back to the
// default (reported) on an unreadable setting rather than silently choosing.
func (w *Worker) deployPolicy() string {
	proj, err := models.GetProjectByID(w.deps.DB.DB, w.projectID)
	if err != nil {
		w.logger.Warn("read deploy policy failed, using default", "error", err, "default", defaultDeployPolicy)
		return defaultDeployPolicy
	}
	switch p := models.ProjectDeployPolicy(proj); p {
	case policyShadow, policyStrict, policyForce:
		return p
	default:
		return defaultDeployPolicy
	}
}

// blockingCategorySummary renders "runtime x2, websocket" for a log line, ordered
// stably. The categories, not the volatile messages, are what a shadow-wave scan
// aggregates on.
func blockingCategorySummary(issues []Issue) string {
	counts := map[string]int{}
	var order []string
	for _, is := range issues {
		if counts[is.Category] == 0 {
			order = append(order, is.Category)
		}
		counts[is.Category]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, c := range order {
		if counts[c] > 1 {
			parts = append(parts, c+" x"+strconv.Itoa(counts[c]))
			continue
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, ", ")
}
