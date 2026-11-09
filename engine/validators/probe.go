package validators

// The one phase-1 validator: the headless-Chrome runtime probe plus the
// websocket smoke. Loads every page in headless Chrome and surfaces uncaught
// JS errors, unhandled rejections, console.error calls, and failed same-origin
// asset fetches; dials every websocket endpoint and asserts the welcome
// contract. Self-skips gracefully when Chrome is not installed.

import (
	"context"
	"database/sql"

	"openfabrica/probe"
	"openfabrica/store"
)

type runtimeProbeValidator struct{}

func (runtimeProbeValidator) Name() string { return "runtime_probe" }

func (runtimeProbeValidator) Validate(ctx context.Context, pdb *sql.DB, baseURL, hostHeader string) []Issue {
	var issues []Issue
	rt := probe.RunRuntimeValidation(ctx, pdb, baseURL, hostHeader)
	for _, i := range rt.Issues {
		if i.Severity == "error" {
			issues = append(issues, i)
		}
	}
	if rt.Skipped != "" {
		// Surface the skip as a warning so a missing browser does not masquerade
		// as a clean runtime pass. Warning + non-deterministic, so it is logged
		// for visibility but never drives a fix-pass (a build cannot fix a host
		// with no Chrome installed).
		issues = append(issues, Issue{
			Severity: store.SeverityWarning,
			Category: "runtime",
			Message:  "runtime page-load checks were skipped: " + rt.Skipped + " (uncaught and console errors on real pages were not verified).",
		})
	}
	// WebSocket endpoints are invisible to the page-navigating probe (a plain
	// GET answers 426), so a relay that upgrades and then never speaks - or welcomes
	// but never DELIVERS between two peers - would ship green without this. Keep the
	// delivery probe's non-looping warnings too (e.g. authoritative presence keyed on
	// uid), which are surfaced but never drive a fix-pass.
	for _, i := range probe.RunWebSocketSmoke(ctx, pdb, baseURL, hostHeader) {
		if i.Severity == store.SeverityError || i.Severity == store.SeverityWarning {
			issues = append(issues, i)
		}
	}
	// The other realtime/push routes are equally invisible to the page probe: an SSE
	// stream served as the wrong type, a feed that 500s on a bad column, or a push
	// subscribe route that is not wired all ship green without a direct dial.
	for _, run := range []func(context.Context, *sql.DB, string, string) []probe.ValidationIssue{
		probe.RunSSESmoke, probe.RunFeedSmoke, probe.RunPushSmoke,
	} {
		for _, i := range run(ctx, pdb, baseURL, hostHeader) {
			if i.Severity == store.SeverityError || i.Severity == store.SeverityWarning {
				issues = append(issues, i)
			}
		}
	}
	return issues
}

func init() {
	RegisterValidator(runtimeProbeValidator{})
}
