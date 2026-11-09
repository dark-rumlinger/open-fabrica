package models

import (
	"encoding/json"
	"sort"
	"strings"
)

// Per-model admin overrides.
//
// Synced model data can be wrong for a private deployment, be
// missing a price, or lag a vendor's change. An admin has to be able to correct
// a row and have the correction STICK - and a sync overwrites every column it
// manages, so "stick" needs somewhere the sync will not tread.
//
// That somewhere is llm_models.config. Only fields the admin actually edited
// are recorded, so the catalog keeps refreshing everything else: correcting one
// model's price does not freeze its context window at today's value.

// ModelOverrides is the set of per-model fields an admin has corrected by hand.
// Pointer fields throughout: nil means "not overridden, let the catalog win",
// which is a different statement from a zero price or a false capability.
type ModelOverrides struct {
	DisplayName       *string  `json:"display_name,omitempty"`
	ContextTokens     *int     `json:"context_tokens,omitempty"`
	MaxOutputTokens   *int     `json:"max_output_tokens,omitempty"`
	SupportsStreaming *bool    `json:"supports_streaming,omitempty"`
	SupportsTools     *bool    `json:"supports_tools,omitempty"`
	SupportsVision    *bool    `json:"supports_vision,omitempty"`
	SupportsReasoning *bool    `json:"supports_reasoning,omitempty"`
	CostInput         *float64 `json:"cost_input,omitempty"`
	CostOutput        *float64 `json:"cost_output,omitempty"`
	CostCacheRead     *float64 `json:"cost_cache_read,omitempty"`
	CostCacheWrite    *float64 `json:"cost_cache_write,omitempty"`
}

// modelConfig is the llm_models.config envelope. Decoded as a map so unknown
// keys survive a round-trip; only "overrides" is interpreted here.
const modelOverridesKey = "overrides"

// Overrides decodes this row's admin corrections. A malformed blob yields none
// rather than an error: an override is a correction on a row that already works,
// and mangled JSON must not take the model out of service.
func (m *LLMModel) Overrides() ModelOverrides {
	var ov ModelOverrides
	if m == nil || strings.TrimSpace(m.Config) == "" {
		return ov
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(m.Config), &envelope) != nil {
		return ov
	}
	raw, ok := envelope[modelOverridesKey]
	if !ok {
		return ov
	}
	_ = json.Unmarshal(raw, &ov)
	return ov
}

// Apply overlays these overrides onto a freshly discovered row, so a sync
// refreshes everything the admin has NOT corrected and leaves what they have.
func (o ModelOverrides) Apply(d *SyncedModel) {
	if o.DisplayName != nil {
		d.DisplayName = *o.DisplayName
	}
	if o.ContextTokens != nil {
		d.ContextTokens = *o.ContextTokens
	}
	if o.MaxOutputTokens != nil {
		d.MaxOutputTokens = *o.MaxOutputTokens
	}
	if o.SupportsStreaming != nil {
		d.SupportsStreaming = *o.SupportsStreaming
	}
	if o.SupportsTools != nil {
		d.SupportsTools = *o.SupportsTools
	}
	if o.SupportsVision != nil {
		d.SupportsVision = *o.SupportsVision
	}
	if o.SupportsReasoning != nil {
		d.SupportsReasoning = *o.SupportsReasoning
	}
	if o.CostInput != nil {
		d.CostInput = *o.CostInput
	}
	if o.CostOutput != nil {
		d.CostOutput = *o.CostOutput
	}
	if o.CostCacheRead != nil {
		d.CostCacheRead = *o.CostCacheRead
	}
	if o.CostCacheWrite != nil {
		d.CostCacheWrite = *o.CostCacheWrite
	}
}

// IsEmpty reports whether nothing is overridden.
func (o ModelOverrides) IsEmpty() bool { return o == ModelOverrides{} }

// ClearModelOverrides drops every recorded correction, handing the row back to
// the catalog on the next sync. The columns keep their current values until
// that sync runs, so "reset" never blanks a model mid-flight.
func ClearModelOverrides(exec Executor, modelPK int, existingConfig string) error {
	envelope := map[string]json.RawMessage{}
	if strings.TrimSpace(existingConfig) != "" {
		_ = json.Unmarshal([]byte(existingConfig), &envelope)
	}
	delete(envelope, modelOverridesKey)
	out, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = exec.Exec("UPDATE llm_models SET config = ? WHERE id = ?", string(out), modelPK)
	return err
}

// OverriddenFields lists the field names this row has admin corrections for,
// sorted. Surfaced so the UI can mark which numbers are yours rather than the
// catalog's.
func (o ModelOverrides) OverriddenFields() []string {
	blob, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(blob, &m) != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MergeModelOverrides folds newly-edited fields into a model's stored
// overrides, preserving any other keys the config blob carries. Passing a
// wholly empty ModelOverrides CLEARS the record, which is how "reset to
// catalog" is expressed.
func MergeModelOverrides(exec Executor, modelPK int, existingConfig string, add ModelOverrides) error {
	envelope := map[string]json.RawMessage{}
	if strings.TrimSpace(existingConfig) != "" {
		_ = json.Unmarshal([]byte(existingConfig), &envelope)
	}
	var current ModelOverrides
	if raw, ok := envelope[modelOverridesKey]; ok {
		_ = json.Unmarshal(raw, &current)
	}
	if add.DisplayName != nil {
		current.DisplayName = add.DisplayName
	}
	if add.ContextTokens != nil {
		current.ContextTokens = add.ContextTokens
	}
	if add.MaxOutputTokens != nil {
		current.MaxOutputTokens = add.MaxOutputTokens
	}
	if add.SupportsStreaming != nil {
		current.SupportsStreaming = add.SupportsStreaming
	}
	if add.SupportsTools != nil {
		current.SupportsTools = add.SupportsTools
	}
	if add.SupportsVision != nil {
		current.SupportsVision = add.SupportsVision
	}
	if add.SupportsReasoning != nil {
		current.SupportsReasoning = add.SupportsReasoning
	}
	if add.CostInput != nil {
		current.CostInput = add.CostInput
	}
	if add.CostOutput != nil {
		current.CostOutput = add.CostOutput
	}
	if add.CostCacheRead != nil {
		current.CostCacheRead = add.CostCacheRead
	}
	if add.CostCacheWrite != nil {
		current.CostCacheWrite = add.CostCacheWrite
	}

	if current.IsEmpty() {
		delete(envelope, modelOverridesKey)
	} else {
		blob, err := json.Marshal(current)
		if err != nil {
			return err
		}
		envelope[modelOverridesKey] = blob
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = exec.Exec("UPDATE llm_models SET config = ? WHERE id = ?", string(out), modelPK)
	return err
}
