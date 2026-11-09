package events

import "testing"

// TestBusPublishDeliversToTypedSubscriber verifies a typed subscriber receives
// only events of its type, in publish order.
func TestBusPublishDeliversToTypedSubscriber(t *testing.T) {
	bus := NewBus()
	var got []string
	bus.Subscribe(EventChatMessage, func(e Event) {
		got = append(got, e.Payload["k"].(string))
	})
	// A subscriber for a different type must never fire here.
	bus.Subscribe(EventToolFailed, func(e Event) {
		t.Errorf("wrong-type subscriber fired for %s", e.Type)
	})

	bus.Publish(NewEvent(EventChatMessage, 1, map[string]any{"k": "a"}))
	bus.Publish(NewEvent(EventChatMessage, 1, map[string]any{"k": "b"}))

	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected [a b], got %v", got)
	}
}

// TestBusSubscribeAllReceivesEveryType verifies a global subscriber sees events
// of every type.
func TestBusSubscribeAllReceivesEveryType(t *testing.T) {
	bus := NewBus()
	var count int
	bus.SubscribeAll(func(e Event) { count++ })

	bus.Publish(NewEvent(EventChatMessage, 0, nil))
	bus.Publish(NewEvent(EventToolFailed, 0, nil))

	if count != 2 {
		t.Fatalf("expected 2 global deliveries, got %d", count)
	}
}

// TestBusUnsubscribeStopsDelivery verifies a handler stops receiving after
// Unsubscribe.
func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	var count int
	id := bus.Subscribe(EventChatMessage, func(e Event) { count++ })

	bus.Publish(NewEvent(EventChatMessage, 0, nil))
	bus.Unsubscribe(id)
	bus.Publish(NewEvent(EventChatMessage, 0, nil))

	if count != 1 {
		t.Fatalf("expected exactly 1 delivery (before unsubscribe), got %d", count)
	}
}

// TestBusPanickingHandlerIsIsolated verifies a panicking handler neither
// propagates out of Publish nor prevents sibling handlers from running.
func TestBusPanickingHandlerIsIsolated(t *testing.T) {
	bus := NewBus()
	var reached bool
	bus.Subscribe(EventChatMessage, func(e Event) { panic("boom") })
	bus.Subscribe(EventChatMessage, func(e Event) { reached = true })

	// Must not panic.
	bus.Publish(NewEvent(EventChatMessage, 0, nil))

	if !reached {
		t.Fatal("handler registered after a panicking one did not run")
	}
}

// TestBusPublishAssignsMonotonicIDs verifies Publish stamps increasing IDs and
// that NewEvent leaves the ID unset (0).
func TestBusPublishAssignsMonotonicIDs(t *testing.T) {
	bus := NewBus()
	e := NewEvent(EventChatMessage, 0, nil)
	if e.ID != 0 {
		t.Fatalf("NewEvent must not assign an ID, got %d", e.ID)
	}

	bus.Publish(e)
	bus.Publish(NewEvent(EventChatMessage, 0, nil))

	all := bus.Since(0)
	if len(all) != 2 {
		t.Fatalf("expected 2 buffered events, got %d", len(all))
	}
	if all[0].ID == 0 || all[0].ID >= all[1].ID {
		t.Errorf("expected increasing non-zero IDs, got %d then %d", all[0].ID, all[1].ID)
	}
}

// TestBusSinceReturnsOnlyNewerEvents verifies the backfill window filters by ID.
func TestBusSinceReturnsOnlyNewerEvents(t *testing.T) {
	bus := NewBus()
	bus.Publish(NewEvent(EventChatMessage, 0, nil))
	first := bus.Since(0)
	if len(first) != 1 {
		t.Fatalf("expected 1 buffered event, got %d", len(first))
	}
	firstID := first[0].ID

	bus.Publish(NewEvent(EventChatMessage, 0, nil))
	newer := bus.Since(firstID)
	if len(newer) != 1 || newer[0].ID <= firstID {
		t.Fatalf("Since(firstID) should return only the newer event, got %v", newer)
	}
}

// TestBusRingBufferOverwrites verifies the backfill buffer retains only the most
// recent BackfillCapacity events, in ascending ID order, and OldestID tracks the
// first retained event.
func TestBusRingBufferOverwrites(t *testing.T) {
	bus := NewBus()
	for i := 0; i < BackfillCapacity+50; i++ {
		bus.Publish(NewEvent(EventChatMessage, 0, nil))
	}

	all := bus.Since(0)
	if len(all) != BackfillCapacity {
		t.Fatalf("buffer should retain exactly %d events, got %d", BackfillCapacity, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("backfill not in ascending ID order at index %d", i)
		}
	}
	if bus.OldestID() != all[0].ID {
		t.Errorf("OldestID %d != first retained event ID %d", bus.OldestID(), all[0].ID)
	}
}
