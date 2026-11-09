package api

import (
	"strings"
	"testing"

	"openfabrica/contracts"
)

// TestAuthoritativeUsageCarriesReducerContract pins the one moment the reducer
// contract is GUARANTEED to reach the model. A guide rides the prompt only when it
// was selected, so the create result is the only delivery that always happens, and
// it happens immediately before the reducer gets written.
//
// It also gives contracts.MustRender a call site under test. MustRender panics on an
// unknown id by design, but a panic nothing ever triggers protects nothing: without
// this, renaming the contract file would go green in CI and only surface as a create
// result that silently teaches the reducer nothing.
func TestAuthoritativeUsageCarriesReducerContract(t *testing.T) {
	usage := buildWSCreateUsage(wsUsageSpec{Path: "game", Authoritative: true, TickHz: 30, HelperOK: true})

	body := contracts.MustRender(contracts.IDRoomReducer)
	if !strings.Contains(usage, body) {
		t.Fatal("an authoritative create result must carry the room-reducer contract verbatim")
	}
	// Transcluded, not restated: the block must be byte-identical to its single
	// home, or the copy has started drifting.
	for _, probe := range []string{"ctx.random", "ctx.after"} {
		if !strings.Contains(usage, probe) {
			t.Errorf("the create result should carry the contract's %q probe", probe)
		}
	}
}

// minResultContractLineLen is the shortest contract line worth treating as a
// restatement. Short lines ("```js", a bare frame name) recur legitimately.
const minResultContractLineLen = 90

// TestNoContractRestatementInCreateResult extends the one-home-per-fact doctrine to
// the surface it never covered. TestNoContractRestatementInToolProse reads a tool's
// OneLiner, WhenToUse, action summaries and arg descriptions; the pack scanners read
// embedded markdown. Neither reads a tool RESULT, and this builder is the largest
// piece of LLM-facing tool prose in the repo.
//
// Same power and same limit as its pack-side twin: it is a copy-paste ratchet, so a
// pasted contract line fails and a REWORDED one does not. The rewordings that
// actually shipped here - the wrong e2ee blob-chunk shape, a self-contradiction about
// presence - are caught by the anti-pattern scanners in
// engine/tool_prose_drift_test.go, which read this file's literals directly.
//
// A whole-body transclusion is the sanctioned form and passes.
func TestNoContractRestatementInCreateResult(t *testing.T) {
	cs, err := contracts.List()
	if err != nil {
		t.Fatalf("contracts.List: %v", err)
	}
	specs := map[string]wsUsageSpec{
		"relay":         {Path: "chat", HelperOK: true},
		"tick":          {Path: "world", HelperOK: true, TickHz: 30, RoomColumn: "room", MaxClients: 8, WriteToTable: "msgs"},
		"signaling":     {Path: "call", HelperOK: true, Signaling: true, E2EE: true, RoomGated: true, Ephemeral: true, MessageTTL: 60},
		"authoritative": {Path: "game", HelperOK: true, Authoritative: true, TickHz: 20, RequiresAuth: true},
		"matchmaking":   {Path: "lobby", HelperOK: true, Matchmaking: true, GroupSize: 2, RoomColumn: "room", TenantColumn: "tenant_id"},
	}
	checked := 0
	for name, spec := range specs {
		hay := contracts.Normalize(buildWSCreateUsage(spec))
		for _, c := range cs {
			// A verbatim transclusion of the whole contract is the correct shape, not
			// a restatement: skip that contract's individual lines for this result.
			if body := contracts.Normalize(c.Body); body != "" && strings.Contains(hay, body) {
				continue
			}
			for _, raw := range strings.Split(c.Body, "\n") {
				n := contracts.Normalize(raw)
				if len(n) < minResultContractLineLen {
					continue
				}
				checked++
				if strings.Contains(hay, n) {
					t.Errorf("the %s create result restates a line of contract %q instead of pointing at it "+
						"(the guide that transcludes it is already in the system prompt by the time this result "+
						"is read): %q", name, c.ID, n[:min(len(n), 120)])
				}
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d contract lines compared - the corpus looks empty and this guard would pass vacuously", checked)
	}
}

// TestNonAuthoritativeUsageOmitsReducerContract is the other half: a relay or tick
// endpoint has no reducer, so spending the create result's budget on the reducer
// contract would be noise on every endpoint that cannot use it.
func TestNonAuthoritativeUsageOmitsReducerContract(t *testing.T) {
	for _, spec := range []wsUsageSpec{
		{Path: "chat", HelperOK: true},
		{Path: "world", TickHz: 20, HelperOK: true},
	} {
		if strings.Contains(buildWSCreateUsage(spec), "The room reducer") {
			t.Errorf("%q is not authoritative and must not carry the reducer contract", spec.Path)
		}
	}
}
