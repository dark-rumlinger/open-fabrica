package llm

import (
	"openfabrica/db/models"
)

// Discovery -> DB rows.
//
// A provider's own listing is the source of truth. Where it reports a window, an
// output cap or a price (Anthropic, Gemini, OpenRouter, a static row) those
// numbers are used as-is. Where it reports only an id (the plain OpenAI
// envelope), the gaps are filled from a name heuristic, which is a last resort
// and never overwrites a reported value. Anything still unknown stays 0 and the
// admin can correct it per model in the UI.

// SyncedRows converts discovery output into the rows SyncProviderModels writes.
func SyncedRows(discovered []ModelInfo) []models.SyncedModel {
	rows := make([]models.SyncedModel, 0, len(discovered))
	for _, m := range discovered {
		m = FillFromName(m)
		rows = append(rows, models.SyncedModel{
			ID:                m.ID,
			DisplayName:       m.DisplayName,
			ContextTokens:     m.ContextTokens,
			MaxOutputTokens:   m.MaxOutputTokens,
			SupportsStreaming: m.SupportsStreaming,
			SupportsTools:     m.SupportsTools,
			SupportsVision:    m.SupportsVision,
			SupportsReasoning: m.SupportsReasoning,
			CostInput:         m.Cost.Input,
			CostOutput:        m.Cost.Output,
			CostCacheRead:     m.Cost.CacheRead,
			CostCacheWrite:    m.Cost.CacheWrite,
		})
	}
	return rows
}

// FillFromName fills the window and output cap from the model id when the
// listing left them at zero. Reported numbers always win: a value the endpoint
// returned describes the live deployment (a llama-server started with `-c 8192`),
// and a name guess must not talk over it.
func FillFromName(m ModelInfo) ModelInfo {
	ctx, out := nameLimits(m.ID)
	if m.ContextTokens == 0 {
		m.ContextTokens = ctx
	}
	if m.MaxOutputTokens == 0 {
		m.MaxOutputTokens = out
	}
	return m
}
