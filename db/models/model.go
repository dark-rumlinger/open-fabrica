package models

import (
	"database/sql"
	"fmt"
	"time"
)

type LLMModel struct {
	ID          int    `json:"id" db:"id"`
	ProviderID  int    `json:"provider_id" db:"provider_id"`
	ModelID     string `json:"model_id" db:"model_id"`
	DisplayName string `json:"display_name" db:"display_name"`
	// ContextTokens is the total prompt+completion window; MaxOutputTokens the
	// per-response cap. They replaced a single max_tokens column that meant
	// one thing on some transports and the other elsewhere, so no caller could
	// read it safely. 0 = unknown.
	ContextTokens     int  `json:"context_tokens" db:"context_tokens"`
	MaxOutputTokens   int  `json:"max_output_tokens" db:"max_output_tokens"`
	SupportsStreaming bool `json:"supports_streaming" db:"supports_streaming"`
	SupportsTools     bool `json:"supports_tools" db:"supports_tools"`
	SupportsVision    bool `json:"supports_vision" db:"supports_vision"`
	SupportsReasoning bool `json:"supports_reasoning" db:"supports_reasoning"`
	// Cost* are USD per million tokens, as reported by the provider at sync or
	// entered by an admin.
	CostInput      float64   `json:"cost_input" db:"cost_input"`
	CostOutput     float64   `json:"cost_output" db:"cost_output"`
	CostCacheRead  float64   `json:"cost_cache_read" db:"cost_cache_read"`
	CostCacheWrite float64   `json:"cost_cache_write" db:"cost_cache_write"`
	IsDefault      bool      `json:"is_default" db:"is_default"`
	IsEnabled      bool      `json:"is_enabled" db:"is_enabled"`
	Config         string    `json:"config" db:"config"`
	CreatedAt      time.Time `json:"created_at" db:"created_at"`
}

// Model operations

// NewModel is the input to CreateModel/CreateModelTx. A struct rather than a
// positional list because the list had reached nine parameters, two of which
// were adjacent ints (the context window and the output cap) that mean very
// different things - exactly the pair a caller would transpose.
type NewModel struct {
	ModelID     string
	DisplayName string
	// ContextTokens is the total window; MaxOutputTokens the per-response cap.
	// 0 on either means unknown, and a model sync fills it in when the provider reports it.
	ContextTokens     int
	MaxOutputTokens   int
	SupportsStreaming bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsReasoning bool
	IsDefault         bool
}

func CreateModel(db *sql.DB, providerID int, m NewModel) (*LLMModel, error) {
	return CreateModelTx(db, providerID, m)
}

// CreateModelTx creates a model using any Executor (DB or Tx). New models
// are enabled by default, admins disable specific ones via the per-model
// PATCH endpoint when they want to hide them from project creation.
func CreateModelTx(exec Executor, providerID int, m NewModel) (*LLMModel, error) {
	result, err := exec.Exec(
		`INSERT INTO llm_models (provider_id, model_id, display_name, context_tokens, max_output_tokens,
		   supports_streaming, supports_tools, supports_vision, supports_reasoning, is_default)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		providerID, m.ModelID, m.DisplayName, m.ContextTokens, m.MaxOutputTokens,
		m.SupportsStreaming, m.SupportsTools, m.SupportsVision, m.SupportsReasoning, m.IsDefault,
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}
	return &LLMModel{
		ID: int(id), ProviderID: providerID, ModelID: m.ModelID,
		DisplayName: m.DisplayName, ContextTokens: m.ContextTokens,
		MaxOutputTokens:   m.MaxOutputTokens,
		SupportsStreaming: m.SupportsStreaming, SupportsTools: m.SupportsTools,
		SupportsVision: m.SupportsVision, SupportsReasoning: m.SupportsReasoning,
		IsDefault: m.IsDefault, IsEnabled: true,
	}, nil
}

// ModelEdit is the full effective state of a model row after an admin edit.
// A struct rather than a positional list: the call had eleven arguments, four
// of them adjacent float64 prices that no reader could tell apart.
type ModelEdit struct {
	DisplayName       string
	ContextTokens     int
	MaxOutputTokens   int
	SupportsStreaming bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsReasoning bool
	CostInput         float64
	CostOutput        float64
	CostCacheRead     float64
	CostCacheWrite    float64
	IsEnabled         bool
}

// UpdateModelCapabilities writes the editable per-model knobs (display name,
// context window, capability flags, enabled). Used by the admin PATCH model
// endpoint and by SyncProviderModels for the "refresh existing row" branch.
func UpdateModelCapabilities(exec Executor, modelID int, e ModelEdit) error {
	_, err := exec.Exec(
		`UPDATE llm_models SET display_name = ?, context_tokens = ?, max_output_tokens = ?,
		   supports_streaming = ?, supports_tools = ?, supports_vision = ?, supports_reasoning = ?,
		   cost_input = ?, cost_output = ?, cost_cache_read = ?, cost_cache_write = ?, is_enabled = ?
		 WHERE id = ?`,
		e.DisplayName, e.ContextTokens, e.MaxOutputTokens,
		e.SupportsStreaming, e.SupportsTools, e.SupportsVision, e.SupportsReasoning,
		e.CostInput, e.CostOutput, e.CostCacheRead, e.CostCacheWrite, e.IsEnabled, modelID,
	)
	return err
}

func ListModelsByProvider(db *sql.DB, providerID int) ([]LLMModel, error) {
	rows, err := db.Query(
		"SELECT "+modelCols+" FROM llm_models WHERE provider_id = ? ORDER BY id",
		providerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []LLMModel
	for rows.Next() {
		var m LLMModel
		if err := scanModel(rows, &m); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

func DeleteModel(db *sql.DB, id int) error {
	_, err := db.Exec("DELETE FROM llm_models WHERE id = ?", id)
	return err
}

// GetModelByID returns a single model by its primary key.
func GetModelByID(db *sql.DB, id int) (*LLMModel, error) {
	m := &LLMModel{}
	if err := scanModel(db.QueryRow("SELECT "+modelCols+" FROM llm_models WHERE id = ?", id), m); err != nil {
		return nil, err
	}
	return m, nil
}
