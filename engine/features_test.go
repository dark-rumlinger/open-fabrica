package engine

import (
	"context"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
)

func TestParseFeatures(t *testing.T) {
	got, err := parseFeatures("```json\n" + `{"features":[
		{"name":"Wall board","behaviours":["three columns", " ", "cards glide between columns"]},
		{"name":"  "},
		{"name":"Staff page","behaviours":["advance with one tap","undo the last move"]}]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "f1" || got[1].ID != "f2" || got[1].Name != "Staff page" {
		t.Fatalf("features = %+v", got)
	}
	if len(got[0].Behaviours) != 2 {
		t.Errorf("blank behaviours are dropped: %+v", got[0].Behaviours)
	}
	if _, err := parseFeatures("nothing"); err == nil {
		t.Error("an unreadable reply is an error")
	}
}

func coverageFixture() ([]PlanFeature, *Plan) {
	features := []PlanFeature{
		{ID: "f1", Name: "Wall board"},
		{ID: "f2", Name: "Staff page", Behaviours: []string{"undo the last move"}},
		{ID: "f3", Name: "Menu management"},
		{ID: "f4", Name: "Loyalty stamps"},
	}
	plan := &Plan{
		Tasks: []PlanTaskSpec{
			{ID: "board-page", Title: "Board", Covers: []string{"f1"}},
			{ID: "staff-page", Title: "Staff", Covers: []string{"f2", "f3"}},
		},
		Exclusions: LooseStrings{"f4: loyalty is a later phase"},
	}
	return features, plan
}

// A feature is accounted for when a task covers it or an exclusion names it; the
// feedback names exactly the rest, with its behaviours.
func TestFeatureCoverageFeedback(t *testing.T) {
	features, plan := coverageFixture()
	if fb := featureCoverageFeedback(plan, features); fb != "" {
		t.Fatalf("every feature is covered or excluded, got %q", fb)
	}
	plan.Tasks[1].Covers = []string{"f3"}
	fb := featureCoverageFeedback(plan, features)
	if !strings.Contains(fb, "`f2` Staff page: undo the last move") || strings.Contains(fb, "`f3`") || strings.Contains(fb, "`f4`") {
		t.Fatalf("feedback must name only the uncovered feature: %q", fb)
	}
	if featureCoverageFeedback(plan, nil) != "" {
		t.Error("no extracted features means no coverage check")
	}
	// An exclusion may name the feature instead of its id.
	plan.Exclusions = append(plan.Exclusions, "No staff page this run: the owner runs the queue from the board")
	if fb := featureCoverageFeedback(plan, features); fb != "" {
		t.Errorf("an exclusion naming the feature accounts for it, got %q", fb)
	}
}

func TestRenderFeaturesForBuild(t *testing.T) {
	features, plan := coverageFixture()
	plan.Features = features
	got := renderFeaturesForBuild(plan)
	for _, want := range []string{
		"`f1` Wall board (built by: `board-page`)",
		"`f2` Staff page: undo the last move (built by: `staff-page`)",
		"`f4` Loyalty stamps (OUT OF SCOPE: f4: loyalty is a later phase)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if renderFeaturesForBuild(&Plan{}) != "" {
		t.Error("a plan without features renders nothing")
	}
}

// A plan that drops a brief feature is sent back once with the feature named; the
// repaired plan seeds, and the engine attaches the feature list to it.
func TestPlanStage_FeatureCoverageRetry(t *testing.T) {
	features := `{"features":[{"name":"Calculator","behaviours":["bill input"]},{"name":"Split by people","behaviours":["divide the total"]}]}`
	dropped := `{"goal":"g","tasks":[{"id":"home-page","title":"Home page","intent":"calculator at /","done_when":"/ renders","paths":["/"],"covers":["f1"]}]}`
	covered := `{"goal":"g","tasks":[{"id":"home-page","title":"Home page","intent":"calculator at / with split","done_when":"/ renders","paths":["/"],"covers":["f1","f2"]}]}`
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, enginetest.Text(features), enginetest.Text(dropped), enginetest.Text(covered),
	}}
	w, _ := scheduledTestWorker(t, p)
	r := &Run{ID: "run-features", Kind: RunKindBuild}
	if _, err := (planStage{}).Run(context.Background(), w, r); err != nil {
		t.Fatalf("plan run: %v", err)
	}

	var sawCritique bool
	for _, req := range p.Requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "`f2` Split by people: divide the total") && strings.Contains(m.Content, "never be built") {
				sawCritique = true
			}
		}
	}
	if !sawCritique {
		t.Fatal("the dropped feature must be fed back to the planner")
	}
	plan, err := ParsePlan(r.PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Features) != 2 || plan.Features[1].ID != "f2" || len(plan.Tasks[0].Covers) != 2 {
		t.Fatalf("stored plan = %+v", plan)
	}
}
