package models

import "database/sql"

// SyncModelStats reports the outcome of a SyncProviderModels call so the
// admin UI can show a meaningful toast ("added 47, updated 12").
type SyncModelStats struct {
	Added   int
	Updated int
}

// SyncProviderModels reconciles the llm_models rows for providerID against a
// discovered catalog, upsert-by-model_id: existing rows have their metadata
// refreshed but is_enabled / is_default are preserved so picker selections survive.
// New models are inserted disabled by default (OpenRouter alone returns hundreds,
// which would drown project-creation pickers). Models that vanish upstream are left
// alone; admins delete them via the per-model DELETE endpoint.
func SyncProviderModels(db *sql.DB, providerID int, discovered []SyncedModel, defaultEnabled bool) (SyncModelStats, error) {
	stats := SyncModelStats{}
	existing, err := ListModelsByProvider(db, providerID)
	if err != nil {
		return stats, err
	}
	byID := make(map[string]*LLMModel, len(existing))
	for i := range existing {
		byID[existing[i].ModelID] = &existing[i]
	}

	for _, d := range discovered {
		if cur, ok := byID[d.ID]; ok {
			// An admin correction outranks the catalog. Applied per FIELD, so a
			// sync still refreshes everything they have not touched.
			cur.Overrides().Apply(&d)
			_, err := db.Exec(
				`UPDATE llm_models SET display_name = ?, context_tokens = ?, max_output_tokens = ?,
				   supports_streaming = ?, supports_tools = ?, supports_vision = ?, supports_reasoning = ?,
				   cost_input = ?, cost_output = ?, cost_cache_read = ?, cost_cache_write = ?
				 WHERE id = ?`,
				d.DisplayName, d.ContextTokens, d.MaxOutputTokens,
				d.SupportsStreaming, d.SupportsTools, d.SupportsVision, d.SupportsReasoning,
				d.CostInput, d.CostOutput, d.CostCacheRead, d.CostCacheWrite, cur.ID,
			)
			if err != nil {
				return stats, err
			}
			stats.Updated++
			continue
		}
		_, err := db.Exec(
			`INSERT INTO llm_models (provider_id, model_id, display_name, context_tokens, max_output_tokens,
			   supports_streaming, supports_tools, supports_vision, supports_reasoning,
			   cost_input, cost_output, cost_cache_read, cost_cache_write, is_default, is_enabled)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			providerID, d.ID, d.DisplayName, d.ContextTokens, d.MaxOutputTokens,
			d.SupportsStreaming, d.SupportsTools, d.SupportsVision, d.SupportsReasoning,
			d.CostInput, d.CostOutput, d.CostCacheRead, d.CostCacheWrite, defaultEnabled,
		)
		if err != nil {
			return stats, err
		}
		stats.Added++
	}
	return stats, nil
}

// SyncedModel is the input shape for SyncProviderModels, kept here (not
// in the llm package) so this file has no upward dependency on llm.
type SyncedModel struct {
	ID          string
	DisplayName string
	// ContextTokens is the window, MaxOutputTokens the per-response cap. Two
	// fields because vendors report different ones under the same name; see
	// the llm_models column comments.
	ContextTokens     int
	MaxOutputTokens   int
	SupportsStreaming bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsReasoning bool
	// Cost* are USD per million tokens, 0 when unknown.
	CostInput      float64
	CostOutput     float64
	CostCacheRead  float64
	CostCacheWrite float64
}
