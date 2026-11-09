package llm

import (
	"context"
	"database/sql"
	"fmt"

	"openfabrica/internal/providercfg"
	"openfabrica/security"
)

// ResolveDefaultModel returns the default LLM provider+model pair as
// configured in the global DB. The returned Provider is constructed live
// (factory + decrypted key) so it can be called directly. Used by the
// action runner's llm_call / ai_embed paths where no per-project chat
// session exists yet.
func ResolveDefaultModel(globalDB *sql.DB, enc *security.Encryptor) (Provider, string, error) {
	if globalDB == nil {
		return nil, "", fmt.Errorf("global DB is nil")
	}
	var (
		providerName, transport, apiKeyEnc, baseURL string
		modelsURL, configJSON, modelID              string
		requiresKey                                 int
	)
	err := globalDB.QueryRow(`
		SELECT p.name, p.transport, COALESCE(p.api_key_encrypted, ''), COALESCE(p.base_url, ''),
		       COALESCE(p.models_url, ''), COALESCE(p.config, '{}'),
		       m.model_id, p.requires_api_key
		FROM llm_models m
		JOIN providers p ON p.id = m.provider_id
		WHERE m.is_default = 1 AND m.is_enabled = 1
		  AND p.is_enabled = 1
		  AND (p.requires_api_key = 0 OR (p.api_key_encrypted IS NOT NULL AND p.api_key_encrypted != ''))
		ORDER BY m.id LIMIT 1`).
		Scan(&providerName, &transport, &apiKeyEnc, &baseURL, &modelsURL, &configJSON, &modelID, &requiresKey)
	if err != nil {
		return nil, "", fmt.Errorf("no default LLM model: %w", err)
	}

	var apiKey string
	if apiKeyEnc != "" && enc != nil {
		k, derr := enc.Decrypt(apiKeyEnc)
		if derr != nil {
			return nil, "", fmt.Errorf("decrypt provider key: %w", derr)
		}
		apiKey = k
	}

	prov, err := NewProvider(transport, Options{
		Name:      providerName,
		APIKey:    apiKey,
		BaseURL:   baseURL,
		ModelsURL: modelsURL,
		Config:    providercfg.Parse(configJSON),
	})
	if err != nil {
		return nil, "", err
	}
	return prov, modelID, nil
}

// EmbedTexts calls the default LLM provider's embedding model on each input
// text and returns the resulting vectors. Selects the first available
// kind='llm' provider whose config carries an `embedding_model` value (same
// rule LoadCapabilities uses) so callers get a single deterministic choice.
func EmbedTexts(ctx context.Context, globalDB *sql.DB, enc *security.Encryptor, _ *Set, texts []string) ([][]float32, error) {
	if globalDB == nil {
		return nil, fmt.Errorf("global DB is nil")
	}
	rows, err := globalDB.Query(`
		SELECT name, transport, COALESCE(api_key_encrypted,''), COALESCE(base_url,''),
		       COALESCE(models_url,''), COALESCE(config,'{}')
		FROM providers
		WHERE kind = 'llm' AND is_enabled = 1
		  AND (requires_api_key = 0 OR (api_key_encrypted IS NOT NULL AND api_key_encrypted != ''))
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name, transport, apiKeyEnc, baseURL, modelsURL, configJSON string
		if err := rows.Scan(&name, &transport, &apiKeyEnc, &baseURL, &modelsURL, &configJSON); err != nil {
			continue
		}
		cfg := providercfg.Parse(configJSON)
		if cfg.EmbeddingModel == "" {
			continue
		}
		var apiKey string
		if apiKeyEnc != "" && enc != nil {
			k, derr := enc.Decrypt(apiKeyEnc)
			if derr != nil {
				continue
			}
			apiKey = k
		}
		prov, perr := NewProvider(transport, Options{
			Name:      name,
			APIKey:    apiKey,
			BaseURL:   baseURL,
			ModelsURL: modelsURL,
			Config:    cfg,
		})
		if perr != nil {
			continue
		}
		ep, ok := prov.(EmbeddingProvider)
		if !ok {
			continue
		}
		return ep.Embed(ctx, texts, cfg.EmbeddingModel)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no LLM provider with embedding_model configured")
}
