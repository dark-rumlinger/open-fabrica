package observability

// explore: the model's handle on the click-through that the build's final sweep runs on
// every page. test_page only loads a page; this clicks every visible control once and
// reports what threw, what changed nothing, what another element covers and what scrolls
// sideways at phone size. Without it a finding from the sweep could only be "fixed" blind
// and was re-judged only at the end of the build.

import (
	"fmt"

	"openfabrica/internal/flowsnap"
	"openfabrica/probe"
	"openfabrica/store"
	"openfabrica/tools"
)

// DiagExplore clicks through one page of the running project.
type DiagExplore struct {
	PagePath string `arg:"page_path" desc:"Page path to click through (e.g. '/staff')." default:"/"`
}

func (a *DiagExplore) ActionName() string { return "explore" }
func (a *DiagExplore) Doc() string {
	return "Click every visible control on one page in a real browser, once each, and report what threw, what changed nothing, what another element covers, and sideways scroll at phone size. The same check the end-of-build sweep runs: use it to verify a fix to such a finding instead of guessing. Data written by the clicks is rolled back."
}
func (a *DiagExplore) ReadOnly() bool { return true }
func (a *DiagExplore) Example() any   { return &DiagExplore{PagePath: "/staff"} }

func (a *DiagExplore) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := a.PagePath
	if path == "" {
		path = "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	token, _ := mintTestTokenFromAnyUser(ctx)
	runCtx := probe.WithAuthToken(ctx.Context(), token)

	var res probe.RuntimeResult
	var sum probe.ExploreSummary
	skip, restoreErr := flowsnap.Around(runCtx, func() {
		res, sum = probe.ExplorePageAt(runCtx, fmt.Sprintf("http://127.0.0.1:%d", port), domain, path, token)
	})
	if skip != "" {
		res = probe.RuntimeResult{Skipped: skip}
	}
	if res.Skipped != "" {
		return tools.Ok(map[string]any{
			"skipped": res.Skipped,
			"note":    "the page was not explored on this host, which says nothing about the app: do not change code for this",
		}), nil
	}

	data := map[string]any{"page_path": path, "controls_clicked": sum.Clicked}
	if sum.LoadFailed {
		// A page that would not load was not explored: that is not a pass. test_page says
		// why it failed to load.
		data["loaded"] = false
		return &tools.Result{Success: false, Code: "page_not_loaded",
			Error: fmt.Sprintf("%s did not load cleanly in the browser (it threw, never finished loading, or could not be reached), so no control was clicked. Run manage_testing action=test_page on it for the reason, fix that first, then explore again.", path),
			Data:  data}, nil
	}
	if sum.Redirect != "" {
		data["redirected_to"] = sum.Redirect
		data["note"] = fmt.Sprintf("%s redirects to %s, so nothing was clicked: explore the page it lands on, and check the redirect is intended", path, sum.Redirect)
	}
	if sum.Gate != "" {
		data["dismissed_first_run_layer"] = sum.Gate
	}
	if sum.Canvas {
		data["canvas_page"] = true
		data["canvas_note"] = "this page draws on a canvas: a control that only changes the scene reads as a warning here, not an error"
	}
	if restoreErr != nil {
		data["rollback_error"] = "the data changes made by the clicks could not be fully rolled back: " + restoreErr.Error()
	}
	var errs, warns []string
	for _, i := range res.Issues {
		msg := clipMiddle(i.Message, maxFailureBytes)
		if i.Severity == store.SeverityError {
			errs = append(errs, msg)
		} else {
			warns = append(warns, msg)
		}
	}
	if len(warns) > 0 {
		data["warnings"] = warns
	}
	if len(errs) == 0 {
		data["passed"] = true
		return tools.Ok(data), nil
	}
	data["findings"] = errs
	return &tools.Result{Success: false, Error: fmt.Sprintf("%d finding(s) on %s", len(errs), path), Data: data}, nil
}
