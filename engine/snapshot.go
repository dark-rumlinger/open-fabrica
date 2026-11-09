package engine

// Project-state snapshot maintenance for the session loop. Both modes are
// strictly APPEND-ONLY - the engine never rewrites history (provider prompt
// caches key on the serialized prefix, so any mid-history mutation re-bills
// every byte after it). SnapshotBaselineDelta (build tasks): a full baseline at
// session start, then appended deltas. SnapshotFull (live chat): a full snapshot
// appended whenever the project changed since the last.

import (
	"context"

	"openfabrica/config"
	"openfabrica/db/models"
	"openfabrica/engine/snapshotbuild"
	"openfabrica/providers/llm"
)

// Snapshot sentinels mark engine-injected project_state messages. HTML
// comments chosen to be unguessable from organic chat content.
const (
	snapshotBaselineSentinel = "<!--__of_snapshot:baseline__-->"
	snapshotDeltaSentinel    = "<!--__of_snapshot:delta__-->"
	snapshotFullSentinel     = "<!--__of_snapshot:project_state__-->"
)

// ownerContextSentinel marks the user message carrying owner name and
// preferences. It lives in the message stream (not the system prompt) so the
// system prompt stays byte-stable across calendar days. It is byte-stable for
// the project's lifetime; the volatile date is a separate dateContextSentinel
// message so a date rollover never invalidates the cached prefix.
const ownerContextSentinel = "<owner_context>"

// dateContextSentinel marks the volatile "today" message (dateContextMessage).
// It is a trailing, uncached message kept out of the cached prefix, and is
// never persisted into a carried live thread (carryableThread) so each wake
// re-seeds a single fresh date rather than accumulating stale ones.
const dateContextSentinel = "<date_context>"

// sessionSnapshot tracks snapshot diff state for one session segment.
type sessionSnapshot struct {
	mode            SnapshotMode
	lastFingerprint string
	// baselineSet gates the one-time full baseline append; prev is the delta
	// base, rolled forward on every appended change. prevSnap keeps the prior
	// snapshot object so a delta can collapse module lines already shown.
	baselineSet bool
	prev        map[string]string
	prevSnap    *snapshotbuild.ProjectSnapshot
	// baselines counts full baselines emitted this segment, so a re-baseline can
	// frame itself as superseding rather than as task-start. deltas counts change
	// updates since the last baseline - the length of the chain the model would
	// have to replay to know current state.
	baselines int
	deltas    int
	// forceBaseline asks the next refresh for a full snapshot instead of a delta.
	// Set when the delta chain outgrows what the model can replay, keyed on chain
	// length alone (see rebaselineAfterDeltas in session.go).
	forceBaseline bool
}

// Trailers for the two full-snapshot shapes. The session-start baseline is emitted
// once at segment start; a re-baseline supersedes a chain that has grown too long
// to replay, and says so - the model must not read it as historical.
const (
	baselineTrailer = "State at the start of this session. Any earlier project_state messages describe prior work and are historical. Change updates below track what you build from here."

	rebaselineTrailer = "CURRENT full project state - supersedes the session-start snapshot and every change update above it; read this one, not the chain."
)

// refresh keeps the model's project_state view current by appending to the
// transcript. Failures are non-fatal: the session continues on the prior
// snapshot.
func (s *session) refreshSnapshot(ctx context.Context, st *sessionSnapshot, tr *Transcript) {
	if st.mode == SnapshotNone {
		return
	}
	snap, err := snapshotbuild.BuildProjectSnapshot(ctx, s.pdb.Reader(), s.parseCache)
	if err != nil {
		s.logger.Warn("snapshot refresh failed; keeping prior snapshot", "error", err)
		return
	}
	if snap == nil {
		return
	}
	snap.EffectiveCSP = s.effectiveCSP()
	snap.UserFlowsOff = !s.userFlowsEnabled()
	switch st.mode {
	case SnapshotBaselineDelta:
		s.refreshBaselineDelta(st, tr, snap)
	case SnapshotFull:
		s.refreshFull(st, tr, snap)
	}
}

// refreshBaselineDelta appends the full baseline once, then per-change deltas -
// and re-appends a full baseline when forceBaseline says the chain is no longer
// replayable (too many deltas piled up behind the last full snapshot).
func (s *session) refreshBaselineDelta(st *sessionSnapshot, tr *Transcript, snap *snapshotbuild.ProjectSnapshot) {
	fp := snap.Fingerprint()
	if !st.baselineSet || st.forceBaseline {
		first := !st.baselineSet
		st.prev = snap.Entities
		st.prevSnap = snap
		st.baselineSet = true
		st.forceBaseline = false
		st.lastFingerprint = fp
		// The segment's first baseline carries the section explainers; a re-baseline
		// sits in the same append-only transcript, so it carries the rows alone.
		rendered := snap.Render()
		if !first {
			rendered = snap.RenderData()
		}
		if rendered != "" {
			trailer := baselineTrailer
			if !first {
				trailer = rebaselineTrailer
			}
			st.baselines++
			st.deltas = 0
			tr.Append(llm.Message{
				Role:      llm.RoleUser,
				Content:   snapshotBaselineSentinel + "\n<project_state>\n" + rendered + "\n</project_state>\n" + trailer,
				Cacheable: true,
			})
		}
		return
	}
	if fp != "" && fp == st.lastFingerprint {
		return
	}
	if delta := snap.RenderDelta(st.prev, st.prevSnap); delta != "" {
		tr.Append(llm.Message{
			Role:      llm.RoleUser,
			Content:   snapshotDeltaSentinel + "\n<project_state_changes>\n" + delta + "\n</project_state_changes>\nChanges since the previous update.",
			Cacheable: true,
		})
		st.deltas++
	}
	st.prev = snap.Entities
	st.prevSnap = snap
	st.lastFingerprint = fp
}

// refreshFull appends a full snapshot when the project changed since the
// last one.
func (s *session) refreshFull(st *sessionSnapshot, tr *Transcript, snap *snapshotbuild.ProjectSnapshot) {
	fp := snap.Fingerprint()
	if fp != "" && fp == st.lastFingerprint {
		return
	}
	rendered := snap.Render()
	if rendered == "" {
		return
	}
	tr.Append(llm.Message{
		Role:      llm.RoleUser,
		Content:   snapshotFullSentinel + "\n<project_state>\n" + rendered + "\n</project_state>\nCurrent ground truth; supersedes any earlier project_state message above.",
		Cacheable: true,
	})
	st.lastFingerprint = fp
}

// userFlowsEnabled reads the owner's user-flow setting, so project_state can say when the
// tests are off. An unreadable row counts as off, the platform default.
func (s *session) userFlowsEnabled() bool {
	if s.deps == nil || s.deps.DB == nil || s.deps.DB.DB == nil {
		return false
	}
	p, err := models.GetProjectByID(s.deps.DB.DB, s.projectID)
	if err != nil || p == nil {
		return false
	}
	return models.ProjectUserFlowsEnabled(p)
}

// effectiveCSP resolves the policy this project's public pages are served under, so
// project_state can report it when it is not the permissive default. Read per refresh
// rather than once per session: it lives in the GLOBAL db (the project override) plus
// config (the instance default), and an owner who tightens it mid-build should see the
// build start honouring it. Best-effort - an unreadable project row falls back to the
// instance default, which is what the router would serve for that project anyway.
func (s *session) effectiveCSP() string {
	var override string
	if s.deps != nil && s.deps.DB != nil && s.deps.DB.DB != nil {
		if p, err := models.GetProjectByID(s.deps.DB.DB, s.projectID); err == nil && p != nil {
			override = models.ProjectCSP(p)
		}
	}
	instance := ""
	if s.deps != nil {
		instance = s.deps.PublicCSP
	}
	return config.ResolveCSP(override, instance)
}
