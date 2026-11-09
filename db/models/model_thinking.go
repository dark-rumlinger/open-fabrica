package models

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Per-model reasoning switch, per engine stage.
//
// Reasoning is a property of the model, not of the endpoint that serves it, and a
// project picks a model, so the switch lives on the model row: llm_models.config,
// beside the admin's catalog overrides. A model sync never writes that column and
// "reset overrides" only clears the overrides key, so the switch survives both.

// modelThinkingKey is the llm_models.config envelope key holding the stage map.
const modelThinkingKey = "thinking"

// ThinkingStages are the engine stages a model's thinking map may name.
var ThinkingStages = []string{"expand", "plan", "build", "live", "review"}

// Thinking returns the model's per-stage reasoning switch: a stage it names is on or
// off; a stage it leaves out follows the engine's default. A malformed blob yields
// none, like Overrides.
func (m *LLMModel) Thinking() map[string]bool {
	if m == nil || strings.TrimSpace(m.Config) == "" {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(m.Config), &envelope) != nil {
		return nil
	}
	raw, ok := envelope[modelThinkingKey]
	if !ok {
		return nil
	}
	var out map[string]bool
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// modelProbeKey holds the capability probes' verdicts, {"reasoning":"yes",
// "tools":"no"}, for models whose listing does not report those capabilities. A
// sync applies them instead of the listing's default, and a model is not probed
// again for a capability it has a verdict for.
const modelProbeKey = "probe"

// ProbeCaps are the capabilities a probe can settle, each mirrored in a column.
var ProbeCaps = []string{"reasoning", "tools"}

var probeColumns = map[string]string{"reasoning": "supports_reasoning", "tools": "supports_tools"}

// ProbeVerdict returns the stored verdict for one capability; ok is false when the
// model has not been probed for it.
func (m *LLMModel) ProbeVerdict(capability string) (has, ok bool) {
	if m == nil || strings.TrimSpace(m.Config) == "" {
		return false, false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(m.Config), &envelope) != nil {
		return false, false
	}
	var verdicts map[string]string
	if json.Unmarshal(envelope[modelProbeKey], &verdicts) != nil {
		return false, false
	}
	switch verdicts[capability] {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// SetProbeVerdict records one capability's verdict and sets its column to match,
// keeping every other key the config blob carries.
func SetProbeVerdict(exec Executor, modelPK int, existingConfig, capability string, has bool) error {
	column, known := probeColumns[capability]
	if !known {
		return fmt.Errorf("probe: unknown capability %q", capability)
	}
	envelope := map[string]json.RawMessage{}
	if strings.TrimSpace(existingConfig) != "" {
		_ = json.Unmarshal([]byte(existingConfig), &envelope)
	}
	verdicts := map[string]string{}
	_ = json.Unmarshal(envelope[modelProbeKey], &verdicts)
	verdicts[capability] = "no"
	if has {
		verdicts[capability] = "yes"
	}
	blob, err := json.Marshal(verdicts)
	if err != nil {
		return err
	}
	envelope[modelProbeKey] = blob
	out, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = exec.Exec("UPDATE llm_models SET config = ?, "+column+" = ? WHERE id = ?", string(out), has, modelPK)
	return err
}

// ValidateThinking refuses a stage name the engine does not have.
func ValidateThinking(thinking map[string]bool) error {
	for stage := range thinking {
		if !slices.Contains(ThinkingStages, stage) {
			return fmt.Errorf("thinking: unknown stage %q (want one of %s)", stage, strings.Join(ThinkingStages, ", "))
		}
	}
	return nil
}

// SetModelThinking stores the model's stage map, keeping every other key the
// config blob carries. An empty map clears it, handing every stage back to the
// engine default.
func SetModelThinking(exec Executor, modelPK int, existingConfig string, thinking map[string]bool) error {
	if err := ValidateThinking(thinking); err != nil {
		return err
	}
	envelope := map[string]json.RawMessage{}
	if strings.TrimSpace(existingConfig) != "" {
		_ = json.Unmarshal([]byte(existingConfig), &envelope)
	}
	if len(thinking) == 0 {
		delete(envelope, modelThinkingKey)
	} else {
		blob, err := json.Marshal(thinking)
		if err != nil {
			return err
		}
		envelope[modelThinkingKey] = blob
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = exec.Exec("UPDATE llm_models SET config = ? WHERE id = ?", string(out), modelPK)
	return err
}
