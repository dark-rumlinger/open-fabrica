package engine

import (
	"testing"

	"openfabrica/events"
)

// publishStageChange must emit UPPERCASE stage codenames. The admin UI keys
// every stage map (tracker pills, "stage complete" dividers, live-stage
// detection) on EXPAND/PLAN/BUILD/LIVE, and the reload path already
// uppercases. Emitting the raw lowercase run.Stage here silently no-ops every
// consumer - most visibly leaving the optional "Idea" (EXPAND) pill absent
// from the progress bar until a page reload reseeds it.
func TestPublishStageChangeEmitsUppercase(t *testing.T) {
	bus := events.NewBus()
	var got []events.Event
	bus.SubscribeAll(func(e events.Event) { got = append(got, e) })

	w := &Worker{deps: &Deps{Bus: bus}, projectID: 7}

	// Entry stage: no prev_stage key, no duration.
	w.publishStageChange("", StageExpand, 0)
	// Transition: both prev and current present, with the completed stage's
	// elapsed time.
	w.publishStageChange(StageExpand, StagePlan, 1500)

	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}

	if got[0].Type != events.EventEngineStageChange {
		t.Errorf("event type = %q, want %q", got[0].Type, events.EventEngineStageChange)
	}
	if s := got[0].Payload["stage"]; s != "EXPAND" {
		t.Errorf("entry stage = %v, want EXPAND", s)
	}
	if _, ok := got[0].Payload["prev_stage"]; ok {
		t.Errorf("entry event must not carry prev_stage, got %v", got[0].Payload["prev_stage"])
	}

	if s := got[1].Payload["stage"]; s != "PLAN" {
		t.Errorf("stage = %v, want PLAN", s)
	}
	if p := got[1].Payload["prev_stage"]; p != "EXPAND" {
		t.Errorf("prev_stage = %v, want EXPAND", p)
	}
	if d := got[1].Payload["duration_ms"]; d != int64(1500) {
		t.Errorf("duration_ms = %v, want 1500", d)
	}
	if _, ok := got[0].Payload["duration_ms"]; ok {
		t.Errorf("entry event must not carry duration_ms, got %v", got[0].Payload["duration_ms"])
	}
}
