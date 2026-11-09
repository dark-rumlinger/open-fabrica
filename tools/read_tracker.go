package tools

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
)

// ReadCacheKey is the identity key under which an artifact body is tracked for
// read-dedup. Centralised here (rather than inlined at the read handler) so any
// caller that wants to pre-mark a body as delivered - e.g. the build loop after
// inlining a hot module into a pass seed - keys it identically to the read path.
func ReadCacheKey(kind, path string) string { return kind + "\x00" + path }

// ReadFingerprint hashes an artifact's body content into the value stored
// against ReadCacheKey. Keyed on the bytes themselves rather than row metadata
// so a metadata-only touch (a page-asset link, a scope promotion, a rename)
// leaves the fingerprint - and so the read dedup - intact, while any body
// change shifts it and the next read serves in full. The length prefix keeps
// the value self-describing in logs.
func ReadFingerprint(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%d:%x", len(body), sum[:8])
}

// ReadTracker remembers which artifact bodies have already been delivered in
// full to the model during one build/tool-loop session, so a repeat read of an
// unchanged artifact returns a short stub instead of re-sending the whole body -
// keeping context (and the input-token bill) from ballooning with duplicate file
// contents, the dominant cost when a model re-reads the same modules several
// times across a build.
//
// One tracker per loop session, pointed at from ToolContext, so its lifetime is
// the build's. A nil *ReadTracker is inert: every method is safe and reports
// "not seen", so call sites outside the build loop (chat, jobs, tests) need no
// special-casing.
type ReadTracker struct {
	mu   sync.Mutex
	seen map[string]*readState // identity key -> what is believed resident in the model's context
	// editFails counts CONSECUTIVE failed edits (patch anchor-not-found) per key
	// within the session. It piggybacks on the tracker's per-segment lifetime so
	// the write path can break a stuck patch/repair loop - after the second miss
	// on one file, steer decisively to a full re-save instead of letting the model
	// keep patching bytes it can no longer see. A successful write clears the key.
	editFails map[string]int
	// editFailsTotal counts the SAME failures cumulatively per key over the whole
	// session and is NOT cleared by a successful write. The consecutive streak
	// above misses the distributed shape - a model that re-sends no-op patches to
	// one file many times across a build, each reset by an unrelated successful
	// edit in between - so the write path reads this to escalate on the pattern
	// even when the misses never stack up back to back.
	editFailsTotal map[string]int
}

// lineRange is an inclusive [from,to] span of 1-indexed lines.
type lineRange struct{ from, to int }

// readState tracks what has been delivered for one artifact at one fingerprint.
// A body too large to fit one read page can never be delivered "in full" by a
// single get, so residence is tracked as the set of line RANGES actually handed
// to the model: a repeat read of an already-delivered range is stubbed even
// though the whole file was never sent at once. `full` is the fast path for a
// body delivered complete (a small file, or a body pre-inlined into a pass
// seed) and short-circuits every range query. A fingerprint shift (any save)
// discards the state, so the post-edit body always re-serves.
type readState struct {
	fingerprint string
	full        bool
	ranges      []lineRange // merged, sorted; only consulted when !full
}

// covers reports whether the delivered ranges fully contain [from,to].
func (s *readState) covers(from, to int) bool {
	if s.full {
		return true
	}
	for _, r := range s.ranges {
		if r.from <= from && to <= r.to {
			return true
		}
	}
	return false
}

// add merges [from,to] into the delivered ranges, coalescing overlaps and
// adjacencies so containment checks stay a simple linear scan.
func (s *readState) add(from, to int) {
	if from < 1 || to < from {
		return
	}
	merged := append(s.ranges, lineRange{from, to})
	sort.Slice(merged, func(i, j int) bool { return merged[i].from < merged[j].from })
	out := merged[:0]
	for _, r := range merged {
		if n := len(out); n > 0 && r.from <= out[n-1].to+1 {
			if r.to > out[n-1].to {
				out[n-1].to = r.to
			}
			continue
		}
		out = append(out, r)
	}
	s.ranges = out
}

// Eviction-aware dedup: a body is stubbed on EVERY repeat read while it is
// believed resident in the model's context, and re-served in full exactly once
// when it is no longer. Residence is tracked precisely, not guessed:
// RecordDelivered marks a body resident, Forget marks it gone. Precise tracking
// (vs a naive "stub once then re-serve" heuristic that re-sends even while the
// body is still resident) avoids the flip-flop that turns a model re-checking
// two modules into a re-read spiral.

// NewReadTracker returns a ready tracker.
func NewReadTracker() *ReadTracker {
	return &ReadTracker{seen: map[string]*readState{}, editFails: map[string]int{}, editFailsTotal: map[string]int{}}
}

// RecordEditFailure increments and returns the consecutive failed-edit count for
// key (a patch whose anchor was not found, or a no-change patch). It also bumps
// the cumulative count read via CumulativeEditFailures. The write path escalates
// to a decisive directive once the consecutive count reaches 2. Nil-safe: returns
// 0 outside the build loop, so a loose tool call or test never escalates spuriously.
func (rt *ReadTracker) RecordEditFailure(key string) int {
	if rt == nil {
		return 0
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.editFails == nil {
		rt.editFails = map[string]int{}
	}
	if rt.editFailsTotal == nil {
		rt.editFailsTotal = map[string]int{}
	}
	rt.editFails[key]++
	rt.editFailsTotal[key]++
	return rt.editFails[key]
}

// CumulativeEditFailures returns the session-total failed-edit count for key,
// which a successful write does NOT reset. The write path reads it to escalate on
// a distributed loop (a file hit with the same patch mistake many times across a
// build, never twice in a row). Nil-safe: 0 outside the build loop.
func (rt *ReadTracker) CumulativeEditFailures(key string) int {
	if rt == nil {
		return 0
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.editFailsTotal[key]
}

// ClearEditFailures resets the CONSECUTIVE failed-edit streak for key, called on a
// successful write so a later isolated miss does not immediately re-escalate. The
// cumulative count is left intact, so a distributed loop still escalates. No-op for nil.
func (rt *ReadTracker) ClearEditFailures(key string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.editFails, key)
}

// ShouldStubRange reports whether a read of [fromLine,toLine] of the body under
// key should be stubbed because that exact span was already delivered to the
// model at this fingerprint (a full delivery covers any span). This is the lever
// for files too large to fit one read page: a model that re-reads the same line
// window - the dominant re-read pattern for a big module - gets a stub instead
// of the bytes it already holds, where the full-body-only dedup never fired
// because such a file is never delivered whole in one get. Pure query.
func (rt *ReadTracker) ShouldStubRange(key, fingerprint string, fromLine, toLine int) bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s := rt.seen[key]
	return s != nil && s.fingerprint == fingerprint && s.covers(fromLine, toLine)
}

// Delivered returns what of the body under key the model holds at this fingerprint:
// all of it (full), or these 1-indexed line ranges. Nothing for an unknown key or a
// different fingerprint. Lets an edit carry residence over to the edited body.
func (rt *ReadTracker) Delivered(key, fingerprint string) (full bool, ranges [][2]int) {
	if rt == nil {
		return false, nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s := rt.seen[key]
	if s == nil || s.fingerprint != fingerprint {
		return false, nil
	}
	for _, r := range s.ranges {
		ranges = append(ranges, [2]int{r.from, r.to})
	}
	return s.full, ranges
}

// Forget marks the body under key as no longer resident in the model's context,
// so the next read serves it in full again (and re-records it). The write path
// calls this for an edit that changed nothing, so the follow-up get re-serves
// the bytes the model needs to see why. A no-op for an unknown key.
func (rt *ReadTracker) Forget(key string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.seen, key)
}

// RecordDelivered marks the full body identified by key as delivered, and so
// resident in the model's context, at the given fingerprint. A changed
// fingerprint (the artifact was edited) overwrites the old entry, so the next
// read re-sends the body in full.
func (rt *ReadTracker) RecordDelivered(key, fingerprint string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.seen[key] = &readState{fingerprint: fingerprint, full: true}
}

// RecordDeliveredRange records that lines [fromLine,toLine] of the body under
// key were delivered at the given fingerprint. Used for a paginated read of a
// large file, where no single get delivers the whole body: each served page
// accumulates into the delivered set so a later read of an already-sent window
// can be stubbed. A fingerprint shift resets the state (the post-edit body is a
// different body and must re-serve).
func (rt *ReadTracker) RecordDeliveredRange(key, fingerprint string, fromLine, toLine int) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s := rt.seen[key]
	if s == nil || s.fingerprint != fingerprint {
		s = &readState{fingerprint: fingerprint}
		rt.seen[key] = s
	}
	if s.full {
		return
	}
	s.add(fromLine, toLine)
}
