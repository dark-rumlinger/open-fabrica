// Per-model capability store: one place recording what a given (provider, model)
// pair will accept, replacing the per-client latches each transport used to keep
// privately.
//
// Two sources feed it: the llm_models rows declare what the synced data says, and
// a live 400 teaches what the endpoint actually enforces. The wire always wins,
// since a vendor can change the rules mid-flight and a stale admin checkbox must
// not re-enable a field the endpoint just rejected.
//
// Keyed on providers.name, which is UNIQUE per row and therefore implies the
// base_url: endpoint scoping without re-deriving URL normalization, and two rows
// pointing at the same model name (direct vs. proxied) learn independently.
//
// Scope limit, enforced by TestEveryKnownModelCapHasASource: this store holds
// tri-state booleans only, read only from buildRequestBody-shaped functions.
// Anything needing a number, an alternate URL, an alternate schema, or a different
// response parse is transport code by construction.
package llm

import "sync"

// Support is what is known about one capability. Unknown is the zero value and
// means nothing has spoken yet, which callers treat as an optimistic yes.
type Support int8

const (
	Unknown Support = iota
	Yes
	No
)

// Capability ids. Closed set: ValidateModelCaps rejects anything outside it at
// boot, so a typo in a quirk rule fails startup rather than silently never firing.
const (
	CapTools  = "tools"  // llm_models.supports_tools
	CapVision = "vision" // llm_models.supports_vision
	// CapReasoning is whether the model reasons AT ALL (llm_models.supports_reasoning).
	// Distinct from CapReasoningEffort, which is whether it accepts OpenAI's
	// reasoning_effort FIELD - a model can reason without taking that knob.
	CapReasoning         = "reasoning"
	CapStreaming         = "streaming"           // llm_models.supports_streaming
	CapStructuredOutput  = "structured_output"   // response_format / forced tool / native format
	CapParallelToolCalls = "parallel_tool_calls" // OpenAI parallel_tool_calls
	CapSamplingParams    = "sampling_params"     // temperature, top_p, top_k
	CapMaxTokensField    = "max_tokens_field"    // OpenAI max_tokens (reasoning models want max_completion_tokens)
	CapThinkingOff       = "thinking_off"        // whether reasoning can be DISABLED
	// CapThinkingField is whether the endpoint accepts the thinking-toggle FIELD
	// at all. Distinct from CapThinkingOff: a strict OpenAI-style endpoint 400s
	// "unknown parameter" on the field itself, in either direction.
	CapThinkingField   = "thinking_field"
	CapReasoningEffort = "reasoning_effort" // OpenAI-style reasoning_effort field
	CapEffort          = "effort"           // Anthropic output_config.effort
	// CapForcedToolChoice is whether a forced tool_choice (type "tool"/"any") is
	// accepted. The newer Anthropic models (Sonnet 5.5, Opus 5.5, Fable 5.1) 400 on
	// it; the structured-output path then selects its single tool with tool_choice=auto.
	CapForcedToolChoice = "forced_tool_choice"
	CapNativeThinkField = "think_field" // Ollama /api/chat "think"
)

// Explicit prompt caching is deliberately NOT a capability here. Whether a
// transport emits cache_control breakpoints is fixed when the client is
// constructed, before any model is known, so this store's (provider, model) key
// cannot express it. It stays transport code: openai.Client.SetCacheControl.

// KnownModelCaps is the closed set of capability ids.
var KnownModelCaps = map[string]bool{
	CapTools: true, CapVision: true, CapStreaming: true,
	CapStructuredOutput: true, CapParallelToolCalls: true,
	CapSamplingParams: true, CapMaxTokensField: true, CapThinkingOff: true, CapThinkingField: true,
	CapReasoningEffort: true, CapEffort: true, CapReasoning: true,
	CapForcedToolChoice: true, CapNativeThinkField: true,
}

// ModelKey identifies one configured provider row and the model sent to it.
type ModelKey struct {
	Provider string // providers.name, UNIQUE per row
	Model    string
}

// flag keeps the two sources apart so provenance survives a re-declare.
type flag struct {
	declared Support
	learned  Support
}

// effective resolves the two sources. Learned wins: it came from the wire.
func (f flag) effective() Support {
	if f.learned != Unknown {
		return f.learned
	}
	return f.declared
}

type modelCaps struct {
	flags map[string]flag

	// cost is the model's published price, USD per million tokens. Kept here
	// rather than re-read from the DB per call because the budget tally runs
	// against a PROJECT database while llm_models lives in the global one -
	// the store is the only place both are visible at once.
	cost ModelCost

	// limits are llm_models.context_tokens and max_output_tokens. Readable
	// because they are now two columns: the single max_tokens column they
	// replaced meant the output cap on some transports and the context window
	// on others, so any consumer would have been mixing the two.
	limits ModelLimits
}

// ModelLimits is what a model can hold and what it can emit. Zero means unknown
// - callers must treat that as "no constraint I can enforce", not as a cap of
// zero. A synced row can also carry the name-heuristic default (8192) when the
// the provider reported nothing for the model, so a small window here is a guess, not a spec; no
// engine logic keys on it today.
type ModelLimits struct {
	// ContextTokens is the total prompt+completion window.
	ContextTokens int
	// MaxOutputTokens is the per-response cap.
	MaxOutputTokens int
}

// CapStore is the process-global capability record. Global rather than
// per-Client because engine.resolve builds a fresh client every session, so a
// per-client store would re-pay each rejected request once per stage.
type CapStore struct {
	mu   sync.RWMutex
	rows map[ModelKey]*modelCaps
}

// Caps is the store every client and the engine read.
var Caps = NewCapStore()

func NewCapStore() *CapStore {
	return &CapStore{rows: make(map[ModelKey]*modelCaps)}
}

// Supports returns what is known about one capability for one model.
func (s *CapStore) Supports(k ModelKey, cap string) Support {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[k]
	if !ok {
		return Unknown
	}
	return row.flags[cap].effective()
}

// Allowed reports whether a request may carry the field behind cap. Unknown and
// Yes both send: only a definite No suppresses, which preserves the
// send-once-then-learn behaviour for anything nothing has spoken about.
func (s *CapStore) Allowed(k ModelKey, cap string) bool {
	return s.Supports(k, cap) != No
}

// Learned returns only what the wire taught, ignoring declared values. For gates
// where a declared No is not trustworthy enough to act on: the DB columns are
// NOT NULL bools populated by name heuristics, so they cannot distinguish "this
// model cannot" from "we did not recognise the name".
func (s *CapStore) Learned(k ModelKey, cap string) Support {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[k]
	if !ok {
		return Unknown
	}
	return row.flags[cap].learned
}

// Declare records what the synced rows say. Never overwrites a learned value.
func (s *CapStore) Declare(k ModelKey, cap string, v Support) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rowLocked(k)
	f := row.flags[cap]
	f.declared = v
	row.flags[cap] = f
}

// Learn records what the endpoint enforced and reports whether it changed
// anything. The false-on-repeat is what bounds the quirk retry loop.
func (s *CapStore) Learn(k ModelKey, cap string, v Support) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rowLocked(k)
	f := row.flags[cap]
	if f.learned == v {
		return false
	}
	f.learned = v
	row.flags[cap] = f
	return true
}

// DeclareLimits stores a model's context window and output cap.
func (s *CapStore) DeclareLimits(k ModelKey, l ModelLimits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rowLocked(k).limits = l
}

// DeclareCost stores a model's published price.
func (s *CapStore) DeclareCost(k ModelKey, c ModelCost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rowLocked(k).cost = c
}

// Cost returns the model's published price, zero when unknown.
func (s *CapStore) Cost(k ModelKey) ModelCost {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if row, ok := s.rows[k]; ok {
		return row.cost
	}
	return ModelCost{}
}

// Reset empties the store. Tests only: the store is process-global, so test
// bleed is the main hazard and every test touching it needs this in t.Cleanup.
func (s *CapStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = make(map[ModelKey]*modelCaps)
}

func (s *CapStore) rowLocked(k ModelKey) *modelCaps {
	row, ok := s.rows[k]
	if !ok {
		row = &modelCaps{flags: make(map[string]flag)}
		s.rows[k] = row
	}
	return row
}

// boolSupport maps a NOT NULL DB column to a definite Yes or No. There is no
// absent state to model, so it never returns Unknown.
func boolSupport(b bool) Support {
	if b {
		return Yes
	}
	return No
}

// StripImages removes image attachments bound for a model declared unable to
// read them, replacing each with a short text marker so the model is not left
// reasoning about an attachment it cannot see. Returns the request and how many
// were dropped.
//
// A drop rather than an error because callers outside the engine build requests
// directly and have no recovery path: a stripped request still answers, a 400
// does not.
func StripImages(req CompletionRequest) (CompletionRequest, int) {
	dropped := 0
	msgs := make([]Message, len(req.Messages))
	copy(msgs, req.Messages)
	for i := range msgs {
		n := len(msgs[i].Images)
		if n == 0 {
			continue
		}
		dropped += n
		msgs[i].Images = nil
		marker := "[image omitted: this model cannot read images]"
		if msgs[i].Content == "" {
			msgs[i].Content = marker
		} else {
			msgs[i].Content += "\n\n" + marker
		}
	}
	if dropped == 0 {
		return req, 0
	}
	req.Messages = msgs
	return req, dropped
}
