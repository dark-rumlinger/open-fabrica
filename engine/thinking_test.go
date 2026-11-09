package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"openfabrica/db/models"
)

// Every stage defaults to off; a model's own map turns a stage on or off; a spec
// with no ThinkingStage (a utility call) is never overridden.
func TestResolveThinking(t *testing.T) {
	plain := resolvedProvider{}
	model := resolvedProvider{Thinking: map[string]bool{StagePlan: true, StageReview: true, StageBuild: false}}

	for _, c := range []struct {
		name  string
		spec  SessionSpec
		rp    resolvedProvider
		want  bool
		isNil bool
	}{
		{"expand default off", DefaultSpec(StageExpand), plain, false, false},
		{"plan default off", DefaultSpec(StagePlan), plain, false, false},
		{"build default off", DefaultSpec(StageBuild), plain, false, false},
		{"live default off", DefaultSpec(StageLive), plain, false, false},
		{"model turns plan on", DefaultSpec(StagePlan), model, true, false},
		{"model keeps build off", DefaultSpec(StageBuild), model, false, false},
		{"model silent on live", DefaultSpec(StageLive), model, false, false},
		{"utility call stays fixed", SessionSpec{Thinking: &thinkingOff}, model, false, false},
		{"no toggle stays none", SessionSpec{}, model, false, true},
	} {
		got := resolveThinking(&c.spec, c.rp)
		if c.isNil {
			if got != nil {
				t.Errorf("%s: want no toggle, got %v", c.name, *got)
			}
			continue
		}
		if got == nil || *got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	for stage, on := range StageThinkingDefaults() {
		if on {
			t.Errorf("stage %s defaults to thinking on; every stage defaults off", stage)
		}
	}
}

// The stage names a model's map may hold are the ones the engine resolves; a stage
// missing from either side would be one the dialog cannot set or one it sets for nothing.
func TestThinkingStagesMatchModelStages(t *testing.T) {
	var engineStages []string
	for stage := range StageThinkingDefaults() {
		engineStages = append(engineStages, stage)
	}
	slices.Sort(engineStages)
	modelStages := slices.Clone(models.ThinkingStages)
	slices.Sort(modelStages)
	if !slices.Equal(engineStages, modelStages) {
		t.Fatalf("engine stages %v, model stages %v", engineStages, modelStages)
	}
}

// The title prompt lists at most maxAvoidNames taken names, keeping the newest.
func TestTitleUserMsgCapsTakenNames(t *testing.T) {
	var names []string
	for i := 0; i < maxAvoidNames+10; i++ {
		names = append(names, fmt.Sprintf("Name%03d", i))
	}
	msg := titleUserMsg("a tip calculator", names)
	if strings.Contains(msg, "Name000") || !strings.Contains(msg, fmt.Sprintf("Name%03d", maxAvoidNames+9)) {
		t.Errorf("want only the newest %d names, got %q", maxAvoidNames, msg)
	}
}
