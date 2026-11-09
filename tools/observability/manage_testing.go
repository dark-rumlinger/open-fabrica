package observability

import (
	"time"

	"openfabrica/tools"
)

// manage_testing groups the active HTTP/JS probes that back-call the project's
// public server. Split off from manage_diagnostics so the catalogue's "read
// what's broken" surface (errors, health, integrity, unlinked artifacts) stays
// separate from the "actively probe my own server" surface. Each probe costs a
// real HTTP request, so the build loop reaches for them deliberately - to verify
// a page or flow right after building it - rather than as a routine self-check.
//
// The action structs (DiagTestEndpoint, DiagTestURL, DiagTestAuthFlow,
// DiagTestPage, DiagTestModule, DiagTestWebSocket) live in the same package and are
// reused verbatim; their ActionName() values map cleanly onto the new tool.

func init() {
	tools.FlowScriptSaver = func(ctx *tools.ToolContext, source string) (*tools.Result, error) {
		return (&DiagWriteFlows{Source: source}).Run(ctx)
	}
	tools.Define(tools.Tool{
		Name:     "manage_testing",
		Probe:    true,
		Category: "observability",
		// A flow replay allows 180s per flow and a page sweep several pages of browser
		// work, far past the 2-minute default, whose expiry would read as an environment
		// skip rather than a slow test.
		MaxDuration: 10 * time.Minute,
		OneLiner:    "Probe the running server (endpoints, URLs, auth, pages, modules, websockets); write and replay of_test.js user flows.",
		MaxResult:   8000,
		// The admin chat card parses the result for its header ("2 of 4 flows
		// failed", "5 flows, 45 steps"); a 500-byte truncation cuts flow results
		// mid-JSON, so send them whole (they are bounded by MaxResult).
		FullResultInEvents: true,
		Summarize:          tools.SummarizeStripBodies,
		Stages:             []string{"build", "live"},
		// No route to manage_diagnostics here: this prose rides BUILD's always-loaded
		// block, where that tool is NOT preloaded - so the advice cost a ~25K-token
		// cache-busting find_tool hop. LIVE, which does preload it, gets the routing
		// from live/STAGE.md instead.
		WhenToUse: `Use to confirm a specific endpoint, page, or auth flow behaves as intended.
Probe only what already exists - a red on an unbuilt route tells you nothing.
Probe a page once it is built and wired: a runtime error caught warm costs less
than one found after the build. Each action issues a real HTTP request,
so probe deliberately; the test_page check after wiring a page (and as the
end-of-build self-check) is the routine exception.`,
		Actions: []tools.Action{
			&DiagTestEndpoint{},
			&DiagTestURL{},
			&DiagTestAuthFlow{},
			&DiagTestPage{},
			&DiagExplore{},
			&DiagTestModule{},
			&DiagTestWebSocket{},
			&DiagWriteFlows{},
			&DiagTestFlows{},
		},
		Hints: func(action string, _ map[string]any, result *tools.Result) string {
			if result == nil || result.Success {
				return ""
			}
			switch action {
			case "test_endpoint":
				return "Common causes: wrong endpoint_path (check api list), missing auth token, endpoint not yet created, or request body mismatch. For non-/api/ paths use test_url with url_path instead."
			case "test_url":
				return "url_path is used verbatim, e.g. '/' (home) or '/manifest.json'. A /files/<name> miss means no file is saved at that exact path (usually a typo); static files always serve when saved, so verify pages with test_page rather than probing files."
			case "test_auth_flow":
				return "Check: auth endpoint exists, users table has required columns, no duplicate registration."
			}
			return ""
		},
	})
}
