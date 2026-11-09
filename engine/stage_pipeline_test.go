package engine

import "testing"

// Every pipeline entry must resolve to a registered stage that carries its own
// wall-clock cap and start line: the drive loop reads both from Info, so a new
// stage missing either would run uncapped or silently.
func TestPipelineStagesAreComplete(t *testing.T) {
	for _, name := range defaultPipeline {
		s, err := stageByName(name)
		if err != nil {
			t.Fatalf("pipeline stage %q: %v", name, err)
		}
		info := s.Info()
		if info.Timeout <= 0 {
			t.Errorf("stage %q declares no Timeout", name)
		}
		if len(info.StartLines) == 0 {
			t.Errorf("stage %q declares no StartLines", name)
		}
	}
}

// Advance follows defaultPipeline; a named Next is honoured as a jump.
func TestResolveNextFollowsPipeline(t *testing.T) {
	cases := []struct {
		from string
		out  Outcome
		want string
	}{
		{StageExpand, Outcome{Advance: true}, StagePlan},
		{StagePlan, Outcome{Advance: true}, StageBuild},
		{StageBuild, Outcome{Advance: true}, ""},
		{StageExpand, Outcome{Next: StageBuild}, StageBuild},
		{StagePlan, Outcome{Advance: true, Next: StageExpand}, StageBuild},
		{"unknown", Outcome{Advance: true}, ""},
	}
	for _, c := range cases {
		if got := resolveNext(c.from, c.out); got != c.want {
			t.Errorf("resolveNext(%q, %+v) = %q, want %q", c.from, c.out, got, c.want)
		}
	}
}
