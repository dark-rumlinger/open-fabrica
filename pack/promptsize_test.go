package pack

import (
	"strings"
	"testing"
)

// volatileSentinels are the substitutions whose VALUE changes between two runs on the same
// install: the plan's task budget varies with a project's ambition level, and the stack note
// with whether EXPAND ran. The rest (column types, catalogues) are install-scoped, identical
// for every project, so where they sit costs nothing.
var volatileSentinels = []string{"{{TASK_BUDGET}}", "{{AMBITION_DIRECTIVE}}", "{{STACK_NOTE}}", "{{GUIDES_CATALOGUE}}"}

// TestVolatileSentinelsSitAtTheFrameTail pins the placement rule the whole frame is ordered
// by: anything that varies between runs goes LAST, because a prompt cache serves only the
// prefix that matches, and every byte after a substituted value is re-read at full price by
// the next project. The plan frame carried its task budget a quarter of the way in, which put
// ~38KB of prose that is identical for every project downstream of one variable sentence.
//
// The same rule already governs the seed's date context (engine/seed.go). This is its
// system-prompt half.
func TestVolatileSentinelsSitAtTheFrameTail(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	// A sentinel must not appear before this fraction of the frame.
	//
	// On a large frame every point is worth hundreds of bytes, so the threshold sits just
	// under the closest legitimate sentinel (expand's {{AMBITION_DIRECTIVE}}, ~96%) rather
	// than loose. Positions are logged below, so read the real headroom from a run rather
	// than from this number.
	//
	// The catalogues are emptied above, so a sentinel with a large FILL behind it measures
	// closer to the end here than it sits in the real prompt.
	const minPosition = 0.95

	for _, st := range []string{"expand", "plan", "build", "live"} {
		// Sentinels left UNSUBSTITUTED so their position in the frame is visible.
		sys := Assemble(AssembleInput{
			Stage: st, Wave: Wave, Selection: Select(ps, st),
			Sentinels: map[string]string{"ALLOWED_COLUMN_TYPES": "", "TOOL_CATALOGUE": ""},
		}).System
		for _, s := range volatileSentinels {
			at := strings.Index(sys, s)
			if at < 0 {
				continue
			}
			frac := float64(at) / float64(len(sys))
			t.Logf("stage=%-6s %-24s at %5.1f%% of frame (byte %d of %d)", st, s, frac*100, at, len(sys))
			if frac < minPosition {
				t.Errorf("stage %q places %s at %.0f%% of the frame (byte %d of %d).\n"+
					"  Its value varies per run, so every byte after it is re-read at full price by the\n"+
					"  next project. Move it to the tail and leave a pointer where it was.",
					st, s, frac*100, at, len(sys))
			}
		}
	}
}
