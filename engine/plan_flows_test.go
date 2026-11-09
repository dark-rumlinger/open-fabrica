package engine

import (
	"reflect"
	"testing"
)

func TestEnsureUserFlowsTask(t *testing.T) {
	withPages := func() *Plan {
		return &Plan{Tasks: []PlanTaskSpec{
			{ID: "data-layer", Title: "Data layer"},
			{Title: "Home page", Paths: []string{"/", "js/home.js"}},
			{ID: "file-page", Title: "File page", Paths: []string{"/:code"}},
		}}
	}

	p := withPages()
	if !ensureUserFlowsTask(p) {
		t.Fatal("a plan with pages must get the user-flows item")
	}
	last := p.Tasks[len(p.Tasks)-1]
	if last.ID != userFlowsTaskID {
		t.Fatalf("user-flows must be appended last, got %+v", last)
	}
	if want := []string{"data-layer", "home-page", "file-page"}; !reflect.DeepEqual(last.Deps, want) {
		t.Fatalf("deps = %v, want every other task %v", last.Deps, want)
	}
	if ensureUserFlowsTask(p) || len(p.Tasks) != 4 {
		t.Fatal("a second call must not add it again")
	}

	nothing := &Plan{Tasks: []PlanTaskSpec{{ID: "lib", Title: "Shared helpers", Paths: []string{"functions/orders.js"}}}}
	if ensureUserFlowsTask(nothing) || len(nothing.Tasks) != 1 {
		t.Fatal("a plan with neither pages nor an API must be left alone")
	}
	// A headless API owes request-step journeys too.
	api := &Plan{Tasks: []PlanTaskSpec{{ID: "api", Title: "Orders API", Paths: []string{"/api/orders"}}}}
	if !ensureUserFlowsTask(api) || len(api.Tasks) != 2 {
		t.Fatal("a plan that builds an API gets the user-flows item")
	}

	own := withPages()
	own.Tasks = append(own.Tasks, PlanTaskSpec{Title: "Tests", Intent: "write of_test.js covering upload and download"})
	if ensureUserFlowsTask(own) {
		t.Fatal("a plan that already plans of_test.js must not get a duplicate")
	}
}

func TestIsPageRoute(t *testing.T) {
	for p, want := range map[string]bool{
		"/":              true,
		"/about":         true,
		"/:code":         true,
		"/f/:slug":       true,
		"/files/app.js":  false,
		"/api/orders":    false,
		"js/home.js":     false,
		"css/global.css": false,
		"/manifest.json": false,
		"default":        false,
	} {
		if got := isPageRoute(p); got != want {
			t.Errorf("isPageRoute(%q) = %v, want %v", p, got, want)
		}
	}
}

// plannedTasks drops the engine-added user-flows item, for tests that assert on the
// checklist the planner itself wrote.
func plannedTasks(tasks []Task) []Task {
	out := tasks[:0:0]
	for _, t := range tasks {
		if t.ID != userFlowsTaskID {
			out = append(out, t)
		}
	}
	return out
}

// The plan frame names endpoints by bare resource ("orders"), so a headless API
// plan written that way still owes request-step journeys, while a bare table
// with no endpoint does not.
func TestPlanBuildsAPIAcceptsBareEndpointResources(t *testing.T) {
	for name, task := range map[string]PlanTaskSpec{
		"endpoint in text": {ID: "orders", Title: "Orders table and endpoint", Paths: []string{"orders"}},
		"manage_api tool":  {ID: "orders", Title: "Orders", Paths: []string{"orders"}, Tools: []string{"manage_api"}},
		"api in intent":    {ID: "orders", Title: "Orders", Intent: "Expose a REST API over orders", Paths: []string{"orders"}},
		"prefixed path":    {ID: "orders", Title: "Orders", Paths: []string{"/api/orders"}},
	} {
		if !planBuildsAPI(&Plan{Tasks: []PlanTaskSpec{task}}) {
			t.Errorf("%s: plan should count as building an API", name)
		}
	}
	for name, task := range map[string]PlanTaskSpec{
		"bare table only": {ID: "orders", Title: "Orders table", Paths: []string{"orders"}},
		"shared layout":   {ID: "shell", Title: "Shared layout and endpoint styling", Paths: []string{"default"}},
		"no paths":        {ID: "x", Title: "API notes"},
	} {
		if planBuildsAPI(&Plan{Tasks: []PlanTaskSpec{task}}) {
			t.Errorf("%s: plan must not count as building an API", name)
		}
	}
}
