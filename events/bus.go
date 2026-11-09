// Package events is the in-process pub/sub bus for engine and server events,
// with a bounded backfill buffer for reconnecting subscribers.
package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// BackfillCapacity is the number of recent events the bus retains for
// reconnection backfill. Sized for ~10 minutes of heavy engine activity at
// a few events/sec; older events are dropped to keep memory bounded.
const BackfillCapacity = 1024

type HandlerFunc func(Event)

type subscription struct {
	id      uint64
	handler HandlerFunc
}

type Bus struct {
	mu       sync.RWMutex
	handlers map[EventType][]subscription
	global   []subscription
	nextID   atomic.Uint64

	// eventID is a separate counter from subscription IDs so they don't share
	// a sequence and ID 0 stays reserved as "no ID assigned". Per-bus so IDs
	// never bleed across Bus instances.
	eventID atomic.Uint64

	// Ring buffer of recent events for backfill on SSE reconnect.
	bufMu  sync.RWMutex
	buffer []Event
	bufPos int // next write index in `buffer`
	bufN   int // count of valid entries (capped at len(buffer))
}

func NewBus() *Bus {
	return &Bus{
		handlers: make(map[EventType][]subscription),
		buffer:   make([]Event, BackfillCapacity),
	}
}

// Subscribe registers a handler for a specific event type and returns a
// subscription ID that can be used to unsubscribe.
func (b *Bus) Subscribe(eventType EventType, handler HandlerFunc) uint64 {
	id := b.nextID.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], subscription{id: id, handler: handler})
	return id
}

// SubscribeAll registers a handler that receives all events and returns a
// subscription ID that can be used to unsubscribe.
func (b *Bus) SubscribeAll(handler HandlerFunc) uint64 {
	id := b.nextID.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.global = append(b.global, subscription{id: id, handler: handler})
	return id
}

// Unsubscribe removes a handler by its subscription ID.
func (b *Bus) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for eventType, subs := range b.handlers {
		for i, s := range subs {
			if s.id == id {
				b.handlers[eventType] = append(subs[:i], subs[i+1:]...)
				return
			}
		}
	}

	for i, s := range b.global {
		if s.id == id {
			b.global = append(b.global[:i], b.global[i+1:]...)
			return
		}
	}
}

// Publish assigns a monotonic event ID, retains the event in the backfill
// buffer, and forwards to subscribers. Handlers run inline; they must be
// non-blocking.
func (b *Bus) Publish(event Event) {
	event.ID = b.eventID.Add(1)
	b.append(event)

	b.mu.RLock()
	// Copy slices under read lock to avoid holding the lock during handler execution.
	typed := make([]subscription, len(b.handlers[event.Type]))
	copy(typed, b.handlers[event.Type])
	global := make([]subscription, len(b.global))
	copy(global, b.global)
	b.mu.RUnlock()

	// One dispatch path for both subscriber kinds; only the panic log label
	// differs. Typed handlers run before global ones, as before.
	dispatch := func(subs []subscription, label string) {
		for _, s := range subs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error(label, "type", event.Type, "panic", r)
					}
				}()
				s.handler(event)
			}()
		}
	}
	dispatch(typed, "event handler panic")
	dispatch(global, "global event handler panic")
}

// append stores event in the ring buffer.
func (b *Bus) append(event Event) {
	b.bufMu.Lock()
	defer b.bufMu.Unlock()
	b.buffer[b.bufPos] = event
	b.bufPos = (b.bufPos + 1) % len(b.buffer)
	if b.bufN < len(b.buffer) {
		b.bufN++
	}
}

// Since returns events with ID > since, in publish order. Empty slice if
// none. If `since` is older than the oldest retained event, the caller is
// past the backfill window. They get whatever is still buffered, which
// may have a gap before `since`.
//
// The returned slice is a copy; callers are free to mutate it.
func (b *Bus) Since(since uint64) []Event {
	b.bufMu.RLock()
	defer b.bufMu.RUnlock()

	if b.bufN == 0 {
		return nil
	}

	// Walk the ring oldest->newest. Start at the oldest valid entry.
	size := len(b.buffer)
	start := (b.bufPos - b.bufN + size) % size

	out := make([]Event, 0, b.bufN)
	for i := 0; i < b.bufN; i++ {
		e := b.buffer[(start+i)%size]
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out
}

// OldestID returns the smallest event ID currently retained, or 0 if the
// buffer is empty. Clients can detect "you missed too much" by comparing
// their last-seen ID against this.
func (b *Bus) OldestID() uint64 {
	b.bufMu.RLock()
	defer b.bufMu.RUnlock()
	if b.bufN == 0 {
		return 0
	}
	size := len(b.buffer)
	start := (b.bufPos - b.bufN + size) % size
	return b.buffer[start].ID
}
