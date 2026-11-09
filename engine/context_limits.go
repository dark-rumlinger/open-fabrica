package engine

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"openfabrica/providers/llm"
)

// The engine never trusts a catalog entry for a model's window or output cap (a synced
// row can carry a name-heuristic guess), so it learns the real limits from the wire:
// a context-overflow rejection lowers the rollover point for the rest of the worker's
// life, and an output-cap rejection pins the cap for the (provider, model) pair.

// overflowRolloverShare is how much of the last accepted prompt size the learned
// rollover ceiling keeps. Under 1 so the next segment rolls over with room for the
// reply and the tool results that arrive after the trigger is evaluated.
const overflowRolloverShare = 0.8

// overflowRollsOver reports whether a failed call should end the segment as a
// rollover instead of an error: the provider said the prompt does not fit, and this
// segment has already made a call that did, so a reseeded (smaller) segment can
// continue. A segment that never fit anything cannot be helped by reseeding, and that
// failure surfaces unchanged. On a rollover it records the ceiling for later segments.
func (s *session) overflowRollsOver(spec *SessionSpec, st *segState, err error) bool {
	lerr, ok := llm.AsLLMError(err)
	if !ok || lerr.Code != llm.CodeContextOverflow || spec.Reseed == nil || st.lastContextTokens <= 0 {
		return false
	}
	ceiling := int(float64(st.lastContextTokens) * overflowRolloverShare)
	if s.rolloverCeiling == 0 || ceiling < s.rolloverCeiling {
		s.rolloverCeiling = ceiling
	}
	s.logger.Warn("provider rejected the prompt as too large; rolling over",
		"stage", spec.Stage, "last_context_tokens", st.lastContextTokens, "learned_ceiling", s.rolloverCeiling)
	return true
}

// outputCaps holds the per-response output limits learned from rejections, keyed by
// provider name and model. Process-global for the same reason the capability store
// is: every session builds its own client, and a per-session record would re-pay the
// rejected request once per stage.
var outputCaps sync.Map // string -> int

func outputCapKey(k llm.ModelKey) string { return k.Provider + "\x00" + k.Model }

// clampMaxTokens lowers a requested output cap to the one learned for the model.
func clampMaxTokens(k llm.ModelKey, requested int) int {
	if v, ok := outputCaps.Load(outputCapKey(k)); ok {
		if n := v.(int); n > 0 && (requested <= 0 || n < requested) {
			return n
		}
	}
	return requested
}

// outputCapNumber matches the integers in a rejection body; learnOutputCap picks
// among them.
var outputCapNumber = regexp.MustCompile(`\d{3,7}`)

// learnOutputCap reads an output-cap rejection ("max_tokens: 32768 > 8192, which is the
// maximum allowed", "supports at most 16384 completion tokens", "valid range of
// max_tokens is [1, 8192]") and returns the model's real cap, or 0 when err is not one.
// Vendors word it differently, so the cap is the largest number in the message that is
// still below what was requested: the limit is always under the offending request, and
// the request itself (and any larger figure) is excluded by that bound.
func learnOutputCap(err error, requested int) int {
	lerr, ok := llm.AsLLMError(err)
	if !ok || lerr.Code != llm.CodeBadRequest || requested <= 0 {
		return 0
	}
	low := strings.ToLower(lerr.Message)
	if !strings.Contains(low, "max_tokens") && !strings.Contains(low, "max_completion_tokens") &&
		!strings.Contains(low, "output tokens") && !strings.Contains(low, "completion tokens") {
		return 0
	}
	best := 0
	for _, m := range outputCapNumber.FindAllString(lerr.Message, -1) {
		n, convErr := strconv.Atoi(m)
		if convErr == nil && n >= 256 && n < requested && n > best {
			best = n
		}
	}
	return best
}

// rememberOutputCap records a learned cap for the model.
func rememberOutputCap(k llm.ModelKey, n int) {
	outputCaps.Store(outputCapKey(k), n)
}
