package engine

// RepeatGuard screens tool calls before dispatch (Before) and folds outcomes
// into its state after (After), capping wasted turns on a stuck model. A stage
// opts in by setting SessionSpec.Guard; nil means no screening.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"openfabrica/internal/stringutil"
	"openfabrica/tools"
)

// Verdict is a guard's screening decision. Block refuses the call; Reply is
// the JSON the model sees as the tool result; Stop force-stops the session
// after this iteration.
type Verdict struct {
	Block bool
	Reply string
	Stop  bool
}

// GuardState is the per-session-segment mutable state for the guard chain. A
// fresh segment gets a fresh state.
type GuardState struct {
	// failedResult: per (tool+action+path), consecutive failures returning the byte-identical
	// result even as other args vary (edits not moving the outcome). lastResult holds the last
	// failed result so a changed result resets the streak; a successful write clears it.
	failedResult map[string]int
	lastResult   map[string]string
	// failedTotal: consecutive failures per target regardless of error text, the backstop for
	// a model whose every wrong edit yields a different error (which resets failedResult).
	// failureNudges counts nudges a target drew so an unheeded one eventually refuses it.
	failedTotal   map[string]int
	failureNudges map[string]int
	// forcedOnce: a target has spent its one override attempt. A call carrying an advertised
	// override (e.g. force=true) is screened past the streak blocks once, then bounded.
	forcedOnce map[string]bool
	// probeCycles: a read-only probe's consecutive identical failures across the writes taken
	// between attempts; probeResult holds the failure it counts so a changed one resets it.
	// Survives the per-write clear, retired only when the probe passes: ends a non-converging cycle.
	probeResult map[string]string
	probeCycles map[string]int
	// lastCallSig / repeatCount: the same exact call shape on consecutive turns
	// regardless of outcome.
	lastCallSig string
	repeatCount int
	// lastCode: the reject code of the last failure per target. A loop on a STRUCTURAL
	// import-graph reject (cyclic_import / entry_exports) is answered with the whole-graph
	// fix rather than the generic "make one targeted edit" reorient - which, for a cycle,
	// is the very move that keeps the loop going. structuralNudged marks a target that has
	// already drawn that directive, so it fires once and then falls through to the ordinary
	// streak handling.
	lastCode         map[string]string
	structuralNudged map[string]bool
}

// NewGuardState returns an empty state for one session segment.
func NewGuardState() *GuardState { return &GuardState{} }

// RepeatGuard

const (
	// repeatedResultBlockThreshold: consecutive failures of one tool+action+path
	// that return the IDENTICAL result (even with different args) before refusal.
	repeatedResultBlockThreshold = 3
	// repeatedCallStallThreshold: identical consecutive calls (any outcome)
	// before refusal + force-stop.
	repeatedCallStallThreshold = 4
	// repeatedFailureNudgeThreshold: failures of one target (any error text) before a
	// re-orientation nudge. Higher than the result block so a genuine multi-step repair isn't cut.
	repeatedFailureNudgeThreshold = 5
	// maxFailureNudges: nudges one target may draw before it is refused for good.
	maxFailureNudges = 2
	// repeatedProbeCycleThreshold: write-then-probe rounds one probe may fail identically
	// before refusal. Higher than the result block since each round carries a real repair.
	repeatedProbeCycleThreshold = 5
	// structuralCycleNudgeThreshold: consecutive failures on one target whose latest reject
	// is a structural import-graph code before the guard redirects to the whole-graph fix.
	// Lower than the generic nudge because the generic "make ONE targeted edit" advice is,
	// for a cycle, the move that sustains the loop (relocating one symbol at a time).
	structuralCycleNudgeThreshold = 3
)

// structuralCycleCodes are the import-graph rejects an incremental edit cannot resolve:
// relocating one symbol, or routing through a registry / dynamic import, just moves the
// cycle to a different file pair. The fix is always to consolidate the whole shared
// surface at once (see structuralCycleDirective).
var structuralCycleCodes = map[string]bool{
	"cyclic_import": true,
	"entry_exports": true,
}

// structuralCycleDirective is the redirect a looping import-cycle target draws once: stop
// editing one file at a time and consolidate the whole shared surface in a single turn.
const structuralCycleDirective = "This save keeps closing an import cycle, and editing one file at a time - relocating a single symbol, a registry the entry fills in its own body, or a dynamic import - only moves the cycle to a different pair of files. Fix the whole graph in ONE turn instead: put EVERY symbol shared across these files (state, helpers, constants) into the zero-import store module (the file that imports no other project module) and export them there; make the entry a boot-only file that imports the leaves and exports nothing; and wherever two leaves each call into the other, keep one direction as the import and send the other back through the store - its subscribe/onState callback, or a `hooks` object the store declares, the listener assigns and the notifier calls. Save all the affected files together, then retry this one."

// RepeatGuard caps wasted turns on a model stuck re-emitting one call, via four
// signals: an identical failing result on one target, a target failing with a
// different error each time (nudged, then refused), a read-only probe failing
// identically across the writes between attempts, and the same call shape
// issued repeatedCallStallThreshold turns in a row. Only the last force-stops
// the session: the first three are per-target, so refusing the stuck one leaves
// the rest reachable; a stall means the model itself is wedged.
type RepeatGuard struct{}

// Before may refuse the call. args is the parsed argument map (nil when
// unparseable - must tolerate nil).
func (RepeatGuard) Before(st *GuardState, _ *SessionSpec, callName string, args map[string]any) *Verdict {
	sig := exactCallSig(callName, args)

	// Consecutive-repeat counter advances on every screened call.
	if sig == st.lastCallSig {
		st.repeatCount++
	} else {
		st.lastCallSig = sig
		st.repeatCount = 1
	}

	action, _ := args["action"].(string)
	key := resultKey(callName, args)
	path := CallTargetPath(args)
	loc := ""
	if path != "" {
		loc = fmt.Sprintf(" on `%s`", path)
	}

	// An override the tool's own reject advertised makes this a different operation, so let it
	// through the refusal once. The hatch is spent only against a refusal actually standing:
	// an override used before any streak armed was never blocked and mustn't cost the attempt.
	refusing := st.failedResult[key] >= repeatedResultBlockThreshold ||
		st.failedTotal[key] >= repeatedFailureNudgeThreshold
	if forced, _ := args["force"].(bool); forced && refusing && !st.forcedOnce[key] {
		if st.forcedOnce == nil {
			st.forcedOnce = map[string]bool{}
		}
		st.forcedOnce[key] = true
		return nil
	}

	// (0) One target looping on a STRUCTURAL import-graph reject (cyclic_import /
	// entry_exports). An incremental edit cannot fix this - it just relocates the cycle -
	// so the "make one targeted fix" advice below is actively wrong here, and the identical-
	// result block (1) never arms because the cycle path (and its error text) changes each
	// attempt. Redirect ONCE to the whole-graph consolidation, then fall through to the
	// ordinary streak handling (which still force-stops if the model keeps looping).
	if structuralCycleCodes[st.lastCode[key]] && st.failedTotal[key] >= structuralCycleNudgeThreshold && !st.structuralNudged[key] {
		if st.structuralNudged == nil {
			st.structuralNudged = map[string]bool{}
		}
		st.structuralNudged[key] = true
		st.failedTotal[key] = 0 // clean slate so the consolidation attempt isn't pre-blocked
		reply, _ := json.Marshal(map[string]any{
			"success": false,
			"code":    "cyclic_import_reorient",
			"error":   structuralCycleDirective,
		})
		return &Verdict{Block: true, Reply: string(reply)}
	}

	// (1) One tool+action+target returning the IDENTICAL failing result: varying arguments
	// aren't moving the outcome. A blocked call never runs After, so the streak stays frozen
	// and this refusal stands until a success clears it; other targets stay reachable.
	if n := st.failedResult[key]; n >= repeatedResultBlockThreshold {
		reply, _ := json.Marshal(map[string]any{
			"success": false,
			"code":    "repeated_result_block",
			"error": fmt.Sprintf("%s (action=%s)%s failed %d times with the same error while the arguments changed - the change isn't affecting the outcome. Re-read the error above, fix the cause it names, switch tools, or move on.",
				callName, action, loc, n),
		})
		return &Verdict{Block: true, Reply: string(reply)}
	}

	// (2) One tool+action+target failing repeatedly with a DIFFERENT error each time: a model
	// guessing rather than converging, which resets (1)'s streak every turn. Each hit re-orients
	// (block, then reset so a recovery can land); once nudges are spent the target is refused.
	if n := st.failedTotal[key]; n >= repeatedFailureNudgeThreshold {
		if st.failureNudges == nil {
			st.failureNudges = map[string]int{}
		}
		if st.failureNudges[key] >= maxFailureNudges {
			// No reset here: the streak stays at the threshold so this refusal stands
			// until a success on the target clears it.
			reply, _ := json.Marshal(map[string]any{
				"success": false,
				"code":    "repeated_failure_block",
				"error": fmt.Sprintf("%s (action=%s)%s failed ~%d times with changing errors and did not recover after %d re-orientations - this operation is refused now. Switch tools, satisfy it another way, or move on and let validation surface it.",
					callName, action, loc, repeatedFailureNudgeThreshold, maxFailureNudges),
			})
			return &Verdict{Block: true, Reply: string(reply)}
		}
		st.failedTotal[key] = 0 // clean slate so the post-nudge fix isn't pre-blocked
		st.failureNudges[key]++
		target := path
		if target == "" {
			target = "the target"
		}
		reply, _ := json.Marshal(map[string]any{
			"success": false,
			"code":    "repeated_failure_reorient",
			"error": fmt.Sprintf("%s (action=%s)%s failed %d times in a row with a DIFFERENT error each time - you are guessing, not converging. Stop and re-orient: re-read the CURRENT state of %s (for a file, `get` it with fresh=true), re-read the exact error above, then make ONE targeted fix - or move to another task and let validation surface this one.",
				callName, action, loc, repeatedFailureNudgeThreshold, target),
		})
		return &Verdict{Block: true, Reply: string(reply)}
	}

	// (3) A probe reporting the IDENTICAL failure across the writes taken between attempts:
	// the fixes land and change nothing the probe can see. Signals (1) and (2) can't see this
	// (each successful write clears their streaks), so this is the only one that ends an
	// unconverging fix-then-probe cycle. Replaying the failure names what to re-read.
	if n := st.probeCycles[key]; n >= repeatedProbeCycleThreshold {
		reply, _ := json.Marshal(map[string]any{
			"success": false,
			"code":    "repeated_probe_cycle",
			"error": fmt.Sprintf("%s (action=%s)%s still fails IDENTICALLY after %d fix-then-probe rounds - none of those fixes changed the outcome, so the cause is not where you are editing. Stop rewriting: the unchanged failure is %s. Check the arguments and types your code passes into platform calls against their documented signature, or move on and let validation surface this.",
				callName, action, loc, n, stringutil.Truncate(st.probeResult[key], 500)),
		})
		return &Verdict{Block: true, Reply: string(reply)}
	}

	// (4) The same call shape over and over says nothing about one target - the model
	// itself is wedged, so stop the session rather than refuse one operation.
	if st.repeatCount >= repeatedCallStallThreshold {
		reply, _ := json.Marshal(map[string]any{
			"success": false,
			"code":    "repeated_call_stall",
			"error": fmt.Sprintf("%s (action=%s) issued %d times in a row with no change - stopping here. Validation re-surfaces any real breakage as a focused fix, or fix it from chat.",
				callName, action, st.repeatCount),
		})
		return &Verdict{Block: true, Reply: string(reply), Stop: true}
	}
	return nil
}

// After folds one executed call's outcome in. ok is the tool-level success
// (envelope success field AND no Go error).
func (RepeatGuard) After(st *GuardState, _ *SessionSpec, callName string, args map[string]any, result string, ok bool) {
	key := resultKey(callName, args)
	if ok {
		// Progress clears the failure streaks: a successful call carrying a path clears EVERY
		// action's streak on that path (so a save clears a stale patch streak), not just this key.
		st.clearPath(key, CallTargetPath(args))
		// A successful state-changing call (not a read-only probe) can change what a probe
		// observes, so clear stale probe failure streaks. The cycle counter is deliberately
		// NOT cleared here (counting across these writes is what it measures); only a PASSING
		// probe retires it, since that is the evidence a fix landed.
		action, _ := args["action"].(string)
		if !isReadOnlyProbeAction(callName, action) {
			clearProbeStreaks(st)
			return
		}
		delete(st.probeResult, key)
		delete(st.probeCycles, key)
		return
	}
	if st.failedResult == nil {
		st.failedResult = map[string]int{}
		st.lastResult = map[string]string{}
		st.failedTotal = map[string]int{}
	}
	// Identical-result counter: the same failing result recurring for this
	// tool+action+path even as the other arguments change. A changed result resets it.
	if st.lastResult[key] == result {
		st.failedResult[key]++
	} else {
		st.lastResult[key] = result
		st.failedResult[key] = 1
	}
	// Total-failure backstop: every consecutive failure on this target, whether or
	// not the error text changed. Reset only by a success (clearPath) or a nudge.
	st.failedTotal[key]++
	// Remember the latest reject code so a structural import-cycle loop draws the
	// whole-graph directive (signal 0) instead of the generic reorient.
	if st.lastCode == nil {
		st.lastCode = map[string]string{}
	}
	st.lastCode[key] = tools.ResultCode(result)
	// Cycle counter for probes only: the same reading counted across the writes that clear
	// everything else. A changed reading is new information and resets it.
	if action, _ := args["action"].(string); isReadOnlyProbeAction(callName, action) {
		if st.probeCycles == nil {
			st.probeResult = map[string]string{}
			st.probeCycles = map[string]int{}
		}
		if st.probeResult[key] == result {
			st.probeCycles[key]++
		} else {
			st.probeResult[key] = result
			st.probeCycles[key] = 1
		}
	}
}

// clearPath drops every failure/nudge streak for a target (all actions), or just
// the exact key when the call carried no target. Safe on nil maps.
func (st *GuardState) clearPath(exactKey, path string) {
	if path == "" {
		delete(st.failedResult, exactKey)
		delete(st.lastResult, exactKey)
		delete(st.failedTotal, exactKey)
		delete(st.failureNudges, exactKey)
		delete(st.forcedOnce, exactKey)
		delete(st.lastCode, exactKey)
		delete(st.structuralNudged, exactKey)
		return
	}
	suffix := "\x00" + path
	match := func(k string) bool { return strings.HasSuffix(k, suffix) }
	for k := range st.failedResult {
		if match(k) {
			delete(st.failedResult, k)
			delete(st.lastResult, k)
		}
	}
	for k := range st.failedTotal {
		if match(k) {
			delete(st.failedTotal, k)
		}
	}
	for k := range st.failureNudges {
		if match(k) {
			delete(st.failureNudges, k)
		}
	}
	for k := range st.forcedOnce {
		if match(k) {
			delete(st.forcedOnce, k)
		}
	}
	for k := range st.lastCode {
		if match(k) {
			delete(st.lastCode, k)
			delete(st.structuralNudged, k)
		}
	}
}

// Shared helpers

// clearProbeStreaks drops the identical-result streaks for every read-only probe, called
// when a state-changing write succeeds: the write may have fixed what the probe was failing
// on, so its prior failures are stale.
func clearProbeStreaks(st *GuardState) {
	isProbe := func(k string) bool {
		// Keys are tool \x00 action \x00 target.
		parts := strings.SplitN(k, "\x00", 3)
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		return isReadOnlyProbeAction(parts[0], action)
	}
	for k := range st.failedResult {
		if isProbe(k) {
			delete(st.failedResult, k)
			delete(st.lastResult, k)
		}
	}
	for k := range st.failedTotal {
		if isProbe(k) {
			delete(st.failedTotal, k)
		}
	}
	for k := range st.failureNudges {
		if isProbe(k) {
			delete(st.failureNudges, k)
		}
	}
	for k := range st.forcedOnce {
		if isProbe(k) {
			delete(st.forcedOnce, k)
		}
	}
}

// resultKey fingerprints a tool call by name + `action` + TARGET (not full args), so the
// identical-result counter groups every attempt at the same operation on the same artifact
// regardless of the OTHER arguments. The target is last so a success can clear by suffix.
func resultKey(callName string, args map[string]any) string {
	action, _ := args["action"].(string)
	return callName + "\x00" + action + "\x00" + CallTargetPath(args)
}

// CallTargetPath extracts the artifact a call targets, tolerating the several
// field names the tools use. First match wins, so a call names its own target
// before its inputs: manage_api create declares both `endpoint_path` and
// `table_name`, and the endpoint is the artifact it builds. A call naming no
// target keys per action; a tool with a new target field belongs here.
func CallTargetPath(args map[string]any) string {
	for _, k := range []string{"path", "file_path", "page_path", "endpoint_path", "table_name", "task_id", "name"} {
		if v, _ := args[k].(string); v != "" {
			return v
		}
	}
	return ""
}

// exactCallSig fingerprints a tool call by name plus every argument key AND value, so two
// calls collide only when byte-identical. The stall counter keys on it to catch a repeat.
func exactCallSig(toolName string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(toolName))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		switch t := args[k].(type) {
		case string:
			h.Write([]byte(t))
		default:
			if b, err := json.Marshal(t); err == nil {
				h.Write(b)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
