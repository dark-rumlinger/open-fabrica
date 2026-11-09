package snapshotbuild

import (
	"strings"
	"testing"
)

// project_state is the highest-frequency LLM text in the build loop: a snapshot
// refresh runs essentially every iteration, and every write turn appends a
// delta. The rows change every turn; the standing explainer paragraphs never
// do, and RenderDelta reuses Render's per-section formatting (deliberately, so
// the two can't drift in shape), which would re-ship every explainer with every
// delta. These tests pin the split: the baseline teaches, the delta reports.

// explainerFragments are a distinctive slice of each standing explainer. Short enough to
// survive an edit to the surrounding sentence, specific enough that nothing else matches.
var explainerFragments = map[string]string{
	"module contracts":  "Each JS module's export/import surface",
	"page DOM anchors":  "the hooks a page-scope script binds to",
	"design system":     "REUSE these on every page instead of inventing ad-hoc classes",
	"page-scope styles": "This is NOT shared vocabulary",
	"views":             "never seed a view",
	"libraries":         "do NOT hand-write their <script>/<link> tags",
	"files":             "Every file listed here is SAVED and CURRENT",
}

// TestBaselineCarriesEveryExplainer is the positive control. Without it the delta test
// below could pass because a section stopped rendering at all.
func TestBaselineCarriesEveryExplainer(t *testing.T) {
	out := fullSnapshot().Render()
	for name, fragment := range explainerFragments {
		if !strings.Contains(out, fragment) {
			t.Errorf("baseline snapshot is missing the %s explainer (%q).\n"+
				"  The baseline is where these rules live - if it stopped carrying one, the delta "+
				"has nothing to defer to and the rule reaches the model nowhere.", name, fragment)
		}
	}
}

// TestSnapshotDeltaCarriesNoExplainers is the ratchet: a delta reports what changed, and
// nothing else. A new section that writes its explainer directly instead of through
// explain() fails here.
func TestSnapshotDeltaCarriesNoExplainers(t *testing.T) {
	// An empty baseline map makes every entity "changed", so the delta renders every
	// section - the worst case, and the one that would re-ship every paragraph.
	delta := deltaOfEverything()
	if delta == "" {
		t.Fatal("delta is empty; the fixture changed nothing and the test proves nothing")
	}
	for name, fragment := range explainerFragments {
		if strings.Contains(delta, fragment) {
			t.Errorf("delta re-ships the %s explainer (%q).\n"+
				"  It is byte-identical to the baseline's copy, appended on every write turn and "+
				"re-read on every turn after. Route it through explain(&b, mode, ...) so it renders "+
				"only withExplainers.", name, fragment)
		}
	}
}

// TestSnapshotDeltaKeepsTheData: the whole point of a delta is the rows. Cutting the
// paragraphs must not cut what the model actually needs from it.
func TestSnapshotDeltaKeepsTheData(t *testing.T) {
	delta := deltaOfEverything()
	for _, want := range []string{
		"Module contracts", // section headings survive
		"init, reset",      // the export surface
		"--bg",             // design tokens
		"card",             // design classes
		"board",            // DOM anchors
		"css/app.css",      // paths
	} {
		if !strings.Contains(delta, want) {
			t.Errorf("delta lost %q - dropping the explainers must not drop the rows.\n%s", want, delta)
		}
	}
}

// TestSnapshotDeltaKeepsOrphanGuidance: the unlinked-files block is the one exception.
// It is not a standing explainer - it is action guidance about a problem that exists right
// now, and subsetByKeys deliberately carries the orphan list whole for that reason.
func TestSnapshotDeltaKeepsOrphanGuidance(t *testing.T) {
	// fullSnapshot already carries an orphan (js/orphan.js).
	delta := deltaOfEverything()
	if !strings.Contains(delta, "js/orphan.js") {
		t.Error("delta dropped the unlinked-file list; it reports a live problem, not a standing rule")
	}
	if !strings.Contains(delta, "attach") {
		t.Error("delta dropped the fix for an unlinked file, leaving the model a list with no action")
	}
}

// TestSnapshotDeltaCollapsesUnchangedModuleLines: a module record that strictly
// grew emits only its new lines plus a collapse marker; a shrunk or reworded
// record renders in full, so baseline + deltas always replay to current state.
func TestSnapshotDeltaCollapsesUnchangedModuleLines(t *testing.T) {
	prev := fullSnapshot()
	baseline := prev.entityFingerprints()

	grown := fullSnapshot()
	grown.Modules[0].Imports = append(grown.Modules[0].Imports,
		ModuleImport{Spec: "./audio.js", Names: []string{"initAudio"}})
	grown.Entities = grown.entityFingerprints()
	delta := grown.RenderDelta(baseline, prev)
	if !strings.Contains(delta, "./audio.js") {
		t.Errorf("delta lost the added import line:\n%s", delta)
	}
	if strings.Contains(delta, "./input.js") {
		t.Errorf("delta re-emits an unchanged import line the model already has:\n%s", delta)
	}
	if !strings.Contains(delta, "unchanged line(s) as last listed") {
		t.Errorf("delta collapsed lines without saying so:\n%s", delta)
	}
	if delta != grown.RenderDelta(baseline, prev) {
		t.Error("delta render is not deterministic")
	}

	shrunk := fullSnapshot()
	shrunk.Modules[0].Imports = shrunk.Modules[0].Imports[:1]
	shrunk.Entities = shrunk.entityFingerprints()
	d2 := shrunk.RenderDelta(baseline, prev)
	if !strings.Contains(d2, "./input.js") {
		t.Errorf("a shrunk record must render in full (a collapsed removal is invisible):\n%s", d2)
	}
	if strings.Contains(d2, "unchanged line(s) as last listed") {
		t.Errorf("a shrunk record must not collapse:\n%s", d2)
	}

	// A nil prev (first delta of a segment) renders full records.
	if d3 := grown.RenderDelta(baseline, nil); !strings.Contains(d3, "./input.js") {
		t.Errorf("nil prev must render the full record:\n%s", d3)
	}
}

// deltaOfEverything renders the worst-case delta: every entity in fullSnapshot() treated
// as changed. Entities is normally filled by the DB-backed builder, so the fixture needs it
// stamped here before RenderDelta has anything to diff.
func deltaOfEverything() string {
	snap := fullSnapshot()
	snap.Entities = snap.entityFingerprints()
	return snap.RenderDelta(map[string]string{}, nil)
}
