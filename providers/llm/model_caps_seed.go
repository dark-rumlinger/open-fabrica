// Declared capabilities: the llm_models rows feeding the capability store.
//
// One direction only. Discovery (ListModels via SyncProviderModels) writes the
// DB row; the store reads the DB row. Feeding ListModels straight into the store
// as well would race the two paths against each other.
package llm

import (
	"database/sql"

	"openfabrica/db/models"
)

// DeclareFromModelRow copies one llm_models row into the store. Idempotent and
// safe to call per resolve, which is what makes an admin edit take effect
// without a restart.
func (s *CapStore) DeclareFromModelRow(providerName string, m *models.LLMModel) {
	if m == nil {
		return
	}
	k := ModelKey{Provider: providerName, Model: m.ModelID}
	s.Declare(k, CapTools, boolSupport(m.SupportsTools))
	s.Declare(k, CapVision, boolSupport(m.SupportsVision))
	s.Declare(k, CapStreaming, boolSupport(m.SupportsStreaming))
	s.Declare(k, CapReasoning, boolSupport(m.SupportsReasoning))
	s.DeclareLimits(k, ModelLimits{
		ContextTokens:   m.ContextTokens,
		MaxOutputTokens: m.MaxOutputTokens,
	})
	s.DeclareCost(k, ModelCost{
		Input:      m.CostInput,
		Output:     m.CostOutput,
		CacheRead:  m.CostCacheRead,
		CacheWrite: m.CostCacheWrite,
	})
}

// SeedCapsFromDB declares every model of every provider in one pass at boot, so
// the first request of a session already knows what the synced rows say rather
// than discovering it a 400 at a time.
func SeedCapsFromDB(db *sql.DB) error {
	providers, err := models.ListProviders(db)
	if err != nil {
		return err
	}
	for i := range providers {
		rows, err := models.ListModelsByProvider(db, providers[i].ID)
		if err != nil {
			return err
		}
		for j := range rows {
			Caps.DeclareFromModelRow(providers[i].Name, &rows[j])
		}
	}
	return nil
}
