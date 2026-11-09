package models

import "database/sql"

// GetDefaultModelForProvider returns the is_default=1 model for the given provider,
// falling back to the first enabled row. Used by manage_image_gen to pick a default
// per image provider; the system-wide chat default goes through GetDefaultModel.
func GetDefaultModelForProvider(db *sql.DB, providerID int) (*LLMModel, error) {
	m := &LLMModel{}
	err := scanModel(db.QueryRow(
		"SELECT "+modelCols+" FROM llm_models WHERE provider_id = ? AND is_enabled = 1 "+
			"ORDER BY is_default DESC, id ASC LIMIT 1",
		providerID,
	), m)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// selectableWhere is the canonical eligibility predicate for project pickers: a
// model is selectable iff its provider is enabled, the model is enabled, and the
// provider is "available" (keyless or a key stored). Centralised so Catalog,
// GetDefaultModel, and GetFallbackModel cannot drift apart.
const selectableWhere = `
	p.is_enabled = 1
	AND m.is_enabled = 1
	AND p.kind = 'llm'
	AND (p.requires_api_key = 0 OR (p.api_key_encrypted IS NOT NULL AND p.api_key_encrypted != ''))
`

// SelectableModelRow pairs a model with its provider for the catalog UI.
// Returned by ListSelectableModels, single source of truth for what's
// pickable in project creation.
type SelectableModelRow struct {
	Model    LLMModel
	Provider LLMProvider
}

// ListSelectableModels returns every model eligible for project creation,
// ordered by provider name then display name. Used by the admin Catalog
// endpoint and any other "what can the user pick right now" surface.
func ListSelectableModels(db *sql.DB) ([]SelectableModelRow, error) {
	rows, err := db.Query(`
		SELECT ` + modelColsM + `,
		       ` + providerColsP + `
		FROM llm_models m
		JOIN providers p ON p.id = m.provider_id
		WHERE ` + selectableWhere + `
		ORDER BY p.name, m.display_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SelectableModelRow
	for rows.Next() {
		var row SelectableModelRow
		if err := scanModelAndProvider(rows, &row.Model, &row.Provider); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func GetDefaultModel(db *sql.DB) (*LLMModel, *LLMProvider, error) {
	m := &LLMModel{}
	p := &LLMProvider{}
	err := scanModelAndProvider(db.QueryRow(`
		SELECT `+modelColsM+`,
		       `+providerColsP+`
		FROM llm_models m
		JOIN providers p ON p.id = m.provider_id
		WHERE m.is_default = 1 AND `+selectableWhere+`
		ORDER BY m.id ASC
		LIMIT 1
	`), m, p)
	if err != nil {
		return nil, nil, err
	}
	return m, p, nil
}

// GetModelForProject returns the model and provider assigned to a project. It
// intentionally does NOT apply selectableWhere: once a project is pinned to a
// model, that link stays valid even if the model is later disabled, so the build
// can finish. New-project creation goes through ListSelectableModels instead.
func GetModelForProject(db *sql.DB, projectID int) (*LLMModel, *LLMProvider, error) {
	m := &LLMModel{}
	p := &LLMProvider{}
	err := scanModelAndProvider(db.QueryRow(`
		SELECT `+modelColsM+`,
		       `+providerColsP+`
		FROM projects s
		JOIN llm_models m ON m.id = s.llm_model_id
		JOIN providers p ON p.id = m.provider_id
		WHERE s.id = ?
	`, projectID), m, p)
	if err != nil {
		return nil, nil, err
	}
	return m, p, nil
}

// GetFallbackModel returns an alternative selectable model on a different
// provider than excludeProviderID. Prefers tool-capable models. Applies the
// canonical selectability rule so we never fall back to a provider whose
// key is missing or whose models are disabled.
func GetFallbackModel(db *sql.DB, excludeProviderID int) (*LLMModel, *LLMProvider, error) {
	m := &LLMModel{}
	p := &LLMProvider{}
	err := scanModelAndProvider(db.QueryRow(`
		SELECT `+modelColsM+`,
		       `+providerColsP+`
		FROM llm_models m
		JOIN providers p ON p.id = m.provider_id
		WHERE `+selectableWhere+` AND p.id != ? AND m.supports_tools = 1
		ORDER BY m.is_default DESC, m.id ASC
		LIMIT 1
	`, excludeProviderID), m, p)
	if err != nil {
		return nil, nil, err
	}
	return m, p, nil
}
