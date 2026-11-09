package content

import (
	"reflect"
	"testing"

	"openfabrica/tools"
)

func TestCarriedLines(t *testing.T) {
	oldBody := "a\nb\nc\nd\ne"
	newBody := "a\nB\nc\nx\nd\ne" // b replaced, x inserted

	// Held in full: every unchanged line carries to its new number.
	if got := carriedLines(oldBody, newBody, true, nil); !reflect.DeepEqual(got, [][2]int{{1, 1}, {3, 3}, {5, 6}}) {
		t.Errorf("full: %v", got)
	}
	// Held lines 3-5 only (c, d, e): they move to 3, 5, 6.
	if got := carriedLines(oldBody, newBody, false, [][2]int{{3, 5}}); !reflect.DeepEqual(got, [][2]int{{3, 3}, {5, 6}}) {
		t.Errorf("ranged: %v", got)
	}
}

// A file the model held in full, patched in one place with the change echoed, is
// still held in full at its new fingerprint; one it held only partly is not.
func TestCarryResidence(t *testing.T) {
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{ReadTracker: rt}
	key := tools.ReadCacheKey("file", "app.js")
	oldBody := "line1\nline2\nline3\nline4"
	newBody := "line1\nLINE2\nline3\nline4"

	rt.RecordDelivered(key, tools.ReadFingerprint(oldBody))
	carryResidence(ctx, "file", "app.js", oldBody, newBody, [][2]int{{1, 3}})
	if full, _ := rt.Delivered(key, tools.ReadFingerprint(newBody)); !full {
		t.Fatal("held in full before, echo covers the change: held in full after")
	}

	rt2 := tools.NewReadTracker()
	rt2.RecordDeliveredRange(key, tools.ReadFingerprint(oldBody), 1, 1)
	carryResidence(&tools.ToolContext{ReadTracker: rt2}, "file", "app.js", oldBody, newBody, [][2]int{{2, 2}})
	full, ranges := rt2.Delivered(key, tools.ReadFingerprint(newBody))
	if full || !reflect.DeepEqual(ranges, [][2]int{{1, 2}}) {
		t.Fatalf("partly held: full=%v ranges=%v, want lines 1-2 only", full, ranges)
	}
	if !rt2.ShouldStubRange(key, tools.ReadFingerprint(newBody), 1, 2) || rt2.ShouldStubRange(key, tools.ReadFingerprint(newBody), 3, 4) {
		t.Error("a re-read of held lines stubs; unseen lines re-serve")
	}
}
