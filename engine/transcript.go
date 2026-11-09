package engine

// Transcript is the append-only message log of one session segment, and the
// enforcement point for the byte-stability contract: once a message has been
// sent to the provider it is SEALED and never mutated, so the serialized prefix
// stays byte-identical call over call - the property every provider prompt cache
// and the replay cache key on. Overflow is handled by the rollover reseed, never
// by rewriting sent history (mid-run rewriting re-bought the cached prefix at
// full price and dropped context the model still needed).

import (
	"openfabrica/providers/llm"
)

// Transcript holds the ordered messages of one session segment.
type Transcript struct {
	msgs   []llm.Message
	sealed int   // messages [0:sealed) have been sent to the provider
	bytes  []int // serialized byte length per sealed message, recorded at Seal time
}

// NewTranscript starts a transcript from the segment's seed messages.
func NewTranscript(seed ...llm.Message) *Transcript {
	t := &Transcript{}
	t.Append(seed...)
	return t
}

// Append adds messages to the unsealed tail.
func (t *Transcript) Append(ms ...llm.Message) {
	t.msgs = append(t.msgs, ms...)
}

// Len returns the total message count.
func (t *Transcript) Len() int { return len(t.msgs) }

// Messages returns a copy of the message slice for building a request. The
// copy keeps callers from aliasing the transcript's backing array.
func (t *Transcript) Messages() []llm.Message {
	out := make([]llm.Message, len(t.msgs))
	copy(out, t.msgs)
	return out
}

// Seal marks every current message as sent, recording byte lengths so the
// prefix-stability counters stay cheap. Call after each successful provider call.
func (t *Transcript) Seal() {
	for i := t.sealed; i < len(t.msgs); i++ {
		t.bytes = append(t.bytes, messageBytes(t.msgs[i]))
	}
	t.sealed = len(t.msgs)
}

// TotalBytes returns the serialized byte length of every message in the
// transcript (sealed and unsealed), on the same accounting basis as
// StablePrefix. Callers add the fixed system+tools bytes to get the request's
// total_request_bytes diagnostic (stable_prefix_bytes / total_request_bytes
// then shows how much of a request the caches can already serve).
func (t *Transcript) TotalBytes() int {
	n := 0
	for i := 0; i < t.sealed && i < len(t.bytes); i++ {
		n += t.bytes[i]
	}
	for i := t.sealed; i < len(t.msgs); i++ {
		n += messageBytes(t.msgs[i])
	}
	return n
}

// StablePrefix returns the prefix-stability counters for diagnostics: how many
// bytes/messages of the next request are byte-identical to what the provider
// has already seen. Sealed messages are never mutated, so the lengths recorded
// at Seal time are authoritative and nothing is rehashed.
func (t *Transcript) StablePrefix() (stableBytes, stableMsgs int) {
	for i := 0; i < t.sealed && i < len(t.bytes); i++ {
		stableBytes += t.bytes[i]
	}
	return stableBytes, t.sealed
}

// messageBytes returns one message's serialized byte length over its
// request-relevant fields, the accounting basis of every prefix counter.
func messageBytes(m llm.Message) int {
	n := len(m.Role) + 1 + len(m.Content) + 1 + len(m.ToolCallID) + 1
	for _, tc := range m.ToolCalls {
		n += len(tc.ID) + 1 + len(tc.Name) + 1 + len(tc.Arguments) + 1
	}
	for _, img := range m.Images {
		n += len(img.MIMEType) + 1 + len(img.Data) + 1
	}
	return n
}
