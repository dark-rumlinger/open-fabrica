package public

// A data.* event's `table` key must carry the TABLE the row lives in.
//
// It is what manage_triggers filters on (event_filter {"table":"orders"}), so the
// value has to be the name a trigger author can know: the table they created. An
// endpoint's path is a different name that only coincides with the table when the
// endpoint happens to be created at table_name == endpoint_path, which is exactly
// what a canonical example does - so a path/table mix-up reads as correct in every
// example and fires for nobody who named an endpoint after its route.
//
// The endpoint path is published alongside it, under `endpoint`, for a filter that
// wants to tell two endpoints over one table apart.

import (
	"testing"

	"openfabrica/events"
)

// captureRowEvent publishes one row event through a real bus and returns its payload.
func captureRowEvent(t *testing.T, evType events.EventType, physTable, endpointPath string, fields map[string]any) map[string]any {
	t.Helper()
	bus := events.NewBus()
	got := make(chan map[string]any, 1)
	bus.Subscribe(evType, func(e events.Event) {
		got <- e.Payload
	})
	h := &Handler{deps: &Deps{Bus: bus}}
	h.publishRowEvent(evType, 1, rowEventTarget{PhysTable: physTable, Endpoint: endpointPath}, "42", fields)
	select {
	case p := <-got:
		return p
	default:
		t.Fatal("no event published")
		return nil
	}
}

func TestRowEventTableIsThePhysicalTableNotTheEndpointPath(t *testing.T) {
	// The case a path==table example can never distinguish: an endpoint routed at
	// "orders/recent" over the "orders" table.
	p := captureRowEvent(t, events.EventDataInsert, "orders", "orders/recent", map[string]any{"total": 10})

	if p["table"] != "orders" {
		t.Errorf("data.insert payload table = %v, want \"orders\" (the physical table).\n"+
			"manage_triggers documents event_filter {\"table\":\"users\"} against this key, so a trigger keyed on the\n"+
			"table the model created silently never fires when the endpoint path differs from it.", p["table"])
	}
	if p["endpoint"] != "orders/recent" {
		t.Errorf("data.insert payload endpoint = %v, want \"orders/recent\" - the route is still published, "+
			"so a filter can tell two endpoints over one table apart", p["endpoint"])
	}
	if p["id"] != int64(42) {
		t.Errorf("data.insert payload id = %#v, want int64(42)", p["id"])
	}
	if total, _ := p["total"].(int); total != 10 {
		t.Errorf("row fields must merge into the payload: total = %v, want 10", p["total"])
	}
}

// The reserved keys are owned by the publisher: a row column named `table`,
// `endpoint`, or `id` must not be able to overwrite them and spoof a filter match.
func TestRowEventReservedKeysAreNotOverwrittenByRowFields(t *testing.T) {
	p := captureRowEvent(t, events.EventDataUpdate, "orders", "orders/recent", map[string]any{
		"table": "admins", "endpoint": "spoofed", "id": "999",
	})

	if p["table"] != "orders" {
		t.Errorf("a row field overwrote the payload's table key (%v) - a trigger filter could be spoofed by row data", p["table"])
	}
	if p["endpoint"] != "orders/recent" {
		t.Errorf("a row field overwrote the payload's endpoint key (%v)", p["endpoint"])
	}
	if p["id"] != int64(42) {
		t.Errorf("a row field overwrote the payload's id key (%v)", p["id"])
	}
}

// Update and delete get the id as the URL segment. A live client keys its rows by
// the id GET returned (a number), so the event must carry the same type or the
// update lands on a second, string-keyed entry and the page never changes. A slug
// or uuid lookup stays the string it is.
func TestRowEventIDMatchesTheRESTType(t *testing.T) {
	bus := events.NewBus()
	var got []any
	bus.SubscribeAll(func(e events.Event) { got = append(got, e.Payload["id"]) })
	h := &Handler{deps: &Deps{Bus: bus}}
	tgt := rowEventTarget{PhysTable: "orders", Endpoint: "orders"}
	h.publishRowEvent(events.EventDataUpdate, 1, tgt, "7", map[string]any{"stage": "brewing"})
	h.publishRowEvent(events.EventDataDelete, 1, tgt, "7", nil)
	h.publishRowEvent(events.EventDataUpdate, 1, tgt, "summer-menu", nil)
	h.publishRowEvent(events.EventDataUpdate, 1, tgt, "007", nil)
	h.publishRowEvent(events.EventDataInsert, 1, tgt, int64(8), nil)
	want := []any{int64(7), int64(7), "summer-menu", "007", int64(8)}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d id = %#v, want %#v", i, got[i], want[i])
		}
	}
}
