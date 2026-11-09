package engine

import "testing"

// clearStateLeafDeps strips the inverted deps a planner sometimes hangs on a
// zero-import state leaf (a task owning a *.state.js module with no edits), while
// leaving subsystem leaves, spine tasks, and page tasks - which carry real ordering
// deps - untouched.
func TestClearStateLeafDeps(t *testing.T) {
	spine := PlanTaskSpec{ID: "spine", Title: "Entry", Paths: []string{"/", "/files/app.js"}}
	cases := []struct {
		name        string
		task        PlanTaskSpec
		wantCleared bool
	}{
		{"state leaf with a spine dep",
			PlanTaskSpec{ID: "state", Paths: []string{"/files/app.state.js"}, Deps: []string{"spine"}}, true},
		{"subsystem leaf edits the entry",
			PlanTaskSpec{ID: "hud", Paths: []string{"/files/app.hud.js"}, Edits: []string{"/files/app.js"}, Deps: []string{"spine"}}, false},
		{"state leaf that also edits the entry",
			PlanTaskSpec{ID: "state", Paths: []string{"/files/app.state.js"}, Edits: []string{"/files/app.js"}, Deps: []string{"spine"}}, false},
		{"spine owns a route",
			PlanTaskSpec{ID: "spine2", Paths: []string{"/", "/files/app.js"}, Deps: []string{"data"}}, false},
		{"page reads a table",
			PlanTaskSpec{ID: "list", Paths: []string{"/list"}, Deps: []string{"posts"}}, false},
		{"ordinary leaf, not a state module",
			PlanTaskSpec{ID: "util", Paths: []string{"/files/app.util.js"}, Deps: []string{"spine"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Plan{Tasks: []PlanTaskSpec{c.task, spine}}
			clearStateLeafDeps(p)
			cleared := len(p.Tasks[0].Deps) == 0
			if cleared != c.wantCleared {
				t.Errorf("deps cleared = %v, want %v (deps now %v)", cleared, c.wantCleared, p.Tasks[0].Deps)
			}
		})
	}
}

// The normalization is wired into the plan-canonicalization pipeline, so a state
// leaf's inverted dep is gone by the time the plan is persisted.
func TestCanonicalizePlanClearsStateLeafDeps(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Entry", Paths: []string{"/", "/files/app.js"}},
			{ID: "state", Title: "State leaf", Paths: []string{"/files/app.state.js"}, Deps: []string{"spine"}},
		},
	}
	canonicalizePlan(p)
	for _, tk := range p.Tasks {
		if tk.ID == "state" && len(tk.Deps) != 0 {
			t.Errorf("state leaf should carry no deps after canonicalization, got %v", tk.Deps)
		}
	}
}
