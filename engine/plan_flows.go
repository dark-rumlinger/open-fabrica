package engine

import (
	"path"
	"strings"

	"openfabrica/db/models"
	"openfabrica/internal/buildstate"
	"openfabrica/internal/stringutil"
)

// userFlowsTaskID is the id of the user-flow item. User flows are no longer a
// planning-time checklist item; the post-build flow gate (runFlowGate) appends this
// item to an ephemeral plan copy so the model works it last, on the finished app.
const userFlowsTaskID = buildstate.UserFlowsTaskID

// planHasUserFlowsTask reports whether a plan already carries the user-flow item. The
// main build never does; the flow gate's ephemeral plan copy does, which is what turns
// on the build frame's `when:flows` block and the user_flows guide prefetch for that
// one session.
func planHasUserFlowsTask(plan *Plan) bool {
	if plan == nil {
		return false
	}
	for _, t := range plan.Tasks {
		if stringutil.TaskSlug(t.ID) == userFlowsTaskID {
			return true
		}
	}
	return false
}

// ensureUserFlowsTask appends the user-flow item to a plan that builds pages or an API,
// depending on every other task so it is worked last, on the finished app. The flow
// gate calls it on an ephemeral plan copy after the build and repair have settled.
// A plan with neither pages nor an API, or one that already carries such an item,
// is left alone. Reports whether it added the item.
func ensureUserFlowsTask(plan *Plan) bool {
	if plan == nil || (!planBuildsPages(plan) && !planBuildsAPI(plan)) {
		return false
	}
	deps := make([]string, 0, len(plan.Tasks))
	for _, t := range plan.Tasks {
		if stringutil.TaskSlug(t.ID) == userFlowsTaskID || mentionsFlowFile(t) {
			return false
		}
		if s := planTaskSlug(t); s != "" {
			deps = append(deps, s)
		}
	}
	plan.Tasks = append(plan.Tasks, PlanTaskSpec{
		ID:    userFlowsTaskID,
		Title: "User flows: write of_test.js and replay it",
		Intent: "Save the journeys a real person takes through this project with manage_testing action=write_flows " +
			"(recall the user_flows guide's writing-flows section first: the step verbs, what to cover, and how to handle a red flow); it replays them as it saves, so fix what that reveals, then replay once with action=test_flows to check the fixes together, not after each one. " +
			"Keep it small: at most 5 flows. Follow the user_flows guide when a flow stays red; " +
			"the post-build validation replays of_test.js once more and reports any that stay red to the owner as checks that did not pass.",
		DoneWhen: "of_test.js is saved and replayed at least once, with the bugs it clearly revealed fixed; " +
			"or test_flows reported a skip, that its replay budget is used up, or that replays were stopped for lack of progress.",
		Deps: deps,
	})
	return true
}

// dropUserFlowsTasks removes every checklist item that writes or replays of_test.js,
// for a project whose owner turned user-flow tests off. The planner does not see that
// setting and, since every project nominally ships an of_test.js, can plan its own
// "write the user flows" item; left in, it sits on the checklist though nothing acts on
// it. Other tasks' deps on a removed item are dropped too. Reports whether it removed any.
func dropUserFlowsTasks(plan *Plan) bool {
	if plan == nil {
		return false
	}
	removed := map[string]bool{}
	kept := make([]PlanTaskSpec, 0, len(plan.Tasks))
	for _, t := range plan.Tasks {
		if stringutil.TaskSlug(t.ID) == userFlowsTaskID || mentionsFlowFile(t) {
			removed[planTaskSlug(t)] = true
			removed[stringutil.TaskSlug(t.ID)] = true
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == len(plan.Tasks) {
		return false
	}
	for i := range kept {
		deps := kept[i].Deps[:0:0]
		for _, d := range kept[i].Deps {
			if !removed[d] && !removed[stringutil.TaskSlug(d)] {
				deps = append(deps, d)
			}
		}
		kept[i].Deps = deps
	}
	plan.Tasks = kept
	return true
}

// userFlowsEnabled reports whether the owner turned user-flow tests on for this project.
// An unreadable project row counts as off, the platform default.
func (w *Worker) userFlowsEnabled() bool {
	proj, err := models.GetProjectByID(w.deps.DB.DB, w.projectID)
	if err != nil {
		return false
	}
	return models.ProjectUserFlowsEnabled(proj)
}

// planBuildsPages reports whether any task owns a page route.
func planBuildsPages(plan *Plan) bool {
	for _, t := range plan.Tasks {
		for _, p := range t.Paths {
			if isPageRoute(p) {
				return true
			}
		}
	}
	return false
}

// planBuildsAPI reports whether any task owns an API route: a headless API's
// journeys are request steps in of_test.js. The plan frame tells the planner to
// name endpoints by their bare resource ("orders", no /api/ prefix), so a bare path
// counts when the task also says it builds an endpoint (an "endpoint"/"api" in its
// text, or manage_api in its tools); a bare path alone may be a table with no
// route to replay requests against.
func planBuildsAPI(plan *Plan) bool {
	for _, t := range plan.Tasks {
		bare := false
		for _, p := range t.Paths {
			if strings.HasPrefix(strings.TrimSpace(p), "/api/") {
				return true
			}
			bare = bare || isBareResourcePath(p)
		}
		if bare && taskBuildsEndpoint(t) {
			return true
		}
	}
	return false
}

// taskBuildsEndpoint reports whether a task's tools or text say it creates an HTTP
// endpoint rather than only a table.
func taskBuildsEndpoint(t PlanTaskSpec) bool {
	for _, n := range t.Tools {
		if strings.TrimSpace(n) == "manage_api" {
			return true
		}
	}
	text := strings.ToLower(t.Title + " " + t.Intent)
	return containsWord(text, "endpoint") || containsWord(text, "endpoints") || containsWord(text, "api")
}

// isPageRoute reports whether a planned path is a page route ("/", "/about",
// "/:code"): rooted, not under the asset or API prefixes, and without a file
// extension. Asset and module paths are relative or live under /files/.
func isPageRoute(p string) bool {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "/files/") || strings.HasPrefix(p, "/api/") {
		return false
	}
	return path.Ext(path.Base(p)) == ""
}

func mentionsFlowFile(t PlanTaskSpec) bool {
	for _, s := range []string{t.Title, t.Intent, t.DoneWhen} {
		if strings.Contains(strings.ToLower(s), "of_test.js") {
			return true
		}
	}
	return false
}
