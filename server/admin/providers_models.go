package admin

import (
	"fmt"
	"net/http"

	"openfabrica/db/models"
	"openfabrica/engine"
)

// modelResponse is the admin-API shape for a model row. It mirrors the DB
// columns but surfaces supports_vision from Config JSON as a top-level bool
// so the frontend doesn't need to parse Config itself.
type modelResponse struct {
	ID          int    `json:"id"`
	ProviderID  int    `json:"provider_id"`
	ModelID     string `json:"model_id"`
	DisplayName string `json:"display_name"`
	// ContextTokens is the window, MaxOutputTokens the per-response cap.
	ContextTokens     int     `json:"context_tokens"`
	MaxOutputTokens   int     `json:"max_output_tokens"`
	SupportsStreaming bool    `json:"supports_streaming"`
	SupportsTools     bool    `json:"supports_tools"`
	SupportsVision    bool    `json:"supports_vision"`
	SupportsReasoning bool    `json:"supports_reasoning"`
	CostInput         float64 `json:"cost_input"`
	CostOutput        float64 `json:"cost_output"`
	CostCacheRead     float64 `json:"cost_cache_read"`
	CostCacheWrite    float64 `json:"cost_cache_write"`
	IsDefault         bool    `json:"is_default"`
	IsEnabled         bool    `json:"is_enabled"`
	// Overridden names the fields an admin has corrected by hand. The model
	// sync fills everything else and refreshes it on every sync, so this is
	// what tells the two apart in the UI.
	Overridden []string `json:"overridden"`
	// ProbePending names the capabilities a probe has yet to settle; the edit dialog
	// probes on open when it is not empty.
	ProbePending []string `json:"probe_pending,omitempty"`
	// Thinking is the model's per-stage reasoning switch; ThinkingDefaults is what a
	// stage it leaves out does (the engine default), so the edit dialog shows each
	// stage's real state without keeping its own copy of the defaults.
	Thinking         map[string]bool `json:"thinking,omitempty"`
	ThinkingDefaults map[string]bool `json:"thinking_defaults"`
}

func toModelResponse(m models.LLMModel) modelResponse {
	return modelResponse{
		ID:                m.ID,
		ProviderID:        m.ProviderID,
		ModelID:           m.ModelID,
		DisplayName:       m.DisplayName,
		ContextTokens:     m.ContextTokens,
		MaxOutputTokens:   m.MaxOutputTokens,
		SupportsStreaming: m.SupportsStreaming,
		SupportsTools:     m.SupportsTools,
		SupportsVision:    m.SupportsVision,
		SupportsReasoning: m.SupportsReasoning,
		CostInput:         m.CostInput,
		CostOutput:        m.CostOutput,
		CostCacheRead:     m.CostCacheRead,
		CostCacheWrite:    m.CostCacheWrite,
		IsDefault:         m.IsDefault,
		IsEnabled:         m.IsEnabled,
		Overridden:        m.Overrides().OverriddenFields(),
		Thinking:          m.Thinking(),
		ThinkingDefaults:  engine.StageThinkingDefaults(),
	}
}

// Models returns the models available for a provider.
func (h *ProvidersHandler) Models(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	dbModels, err := models.ListModelsByProvider(h.deps.DB.DB, providerID)
	if err != nil {
		h.deps.serverError(w, "failed to list models", err)
		return
	}
	provider, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	resp := make([]modelResponse, 0, len(dbModels))
	for _, m := range dbModels {
		mr := toModelResponse(m)
		mr.ProbePending = probeCapsFor(provider, &m, false)
		resp = append(resp, mr)
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateModel adds a new model to a provider.
func (h *ProvidersHandler) CreateModel(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	var req struct {
		ModelID           string `json:"model_id"`
		DisplayName       string `json:"display_name"`
		ContextTokens     int    `json:"context_tokens"`
		MaxOutputTokens   int    `json:"max_output_tokens"`
		SupportsStreaming bool   `json:"supports_streaming"`
		SupportsTools     bool   `json:"supports_tools"`
		SupportsVision    bool   `json:"supports_vision"`
		SupportsReasoning bool   `json:"supports_reasoning"`
		IsDefault         bool   `json:"is_default"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "model_id is required")
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.ModelID
	}
	if req.ContextTokens == 0 {
		// A hand-added model with no stated window: assume the smallest thing
		// worth serving rather than 0, which downstream would read as unknown.
		req.ContextTokens = 4096
	}

	model, err := models.CreateModel(h.deps.DB.DB, providerID, models.NewModel{
		ModelID:           req.ModelID,
		DisplayName:       req.DisplayName,
		ContextTokens:     req.ContextTokens,
		MaxOutputTokens:   req.MaxOutputTokens,
		SupportsStreaming: req.SupportsStreaming,
		SupportsTools:     req.SupportsTools,
		SupportsVision:    req.SupportsVision,
		SupportsReasoning: req.SupportsReasoning,
		IsDefault:         req.IsDefault,
	})
	if err != nil {
		h.deps.serverError(w, "failed to create model", err)
		return
	}

	// Demote any OTHER system-wide defaults only after the new row exists, so a
	// failed insert never leaves the system with zero defaults. A brief two-
	// defaults window is harmless: GetDefaultModel tolerates it (orders by
	// is_default DESC, id ASC). One default at a time, see SetDefaultModel.
	if req.IsDefault {
		if _, err := h.deps.DB.ExecWrite("UPDATE llm_models SET is_default = 0 WHERE is_default = 1 AND id != ? AND provider_id IN (SELECT id FROM providers WHERE kind = 'llm')", model.ID); err != nil {
			h.deps.Logger.Error("failed to clear previous default models", "error", err)
		}
	}
	writeJSON(w, http.StatusCreated, toModelResponse(*model))
}

// UpdateModel edits an existing model row. Accepts the same fields as
// CreateModel (minus model_id, which is immutable); any field omitted in
// the request falls back to the stored value. Lets operators flip
// supports_vision after the fact without deleting and recreating.
func (h *ProvidersHandler) UpdateModel(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}
	modelPK, ok := requireIntParam(w, r, "modelID", "invalid model ID")
	if !ok {
		return
	}

	existing, err := models.GetModelByID(h.deps.DB.DB, modelPK)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found")
		return
	}
	if existing.ProviderID != providerID {
		writeError(w, http.StatusBadRequest, "model does not belong to this provider")
		return
	}

	// Pointer fields so omitted values don't clobber existing state.
	var req struct {
		DisplayName       *string  `json:"display_name"`
		ContextTokens     *int     `json:"context_tokens"`
		MaxOutputTokens   *int     `json:"max_output_tokens"`
		SupportsStreaming *bool    `json:"supports_streaming"`
		SupportsTools     *bool    `json:"supports_tools"`
		SupportsVision    *bool    `json:"supports_vision"`
		SupportsReasoning *bool    `json:"supports_reasoning"`
		CostInput         *float64 `json:"cost_input"`
		CostOutput        *float64 `json:"cost_output"`
		CostCacheRead     *float64 `json:"cost_cache_read"`
		CostCacheWrite    *float64 `json:"cost_cache_write"`
		IsEnabled         *bool    `json:"is_enabled"`
		// ResetOverrides drops every recorded correction, handing the row back
		// to the catalog on the next sync.
		ResetOverrides bool `json:"reset_overrides"`
		// Thinking replaces the model's per-stage reasoning switch; an empty map
		// clears it back to the engine defaults. Not a catalog override: a sync and
		// a reset both leave it alone.
		Thinking map[string]bool `json:"thinking"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := models.ValidateThinking(req.Thinking); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	edit := models.ModelEdit{
		DisplayName:       existing.DisplayName,
		ContextTokens:     existing.ContextTokens,
		MaxOutputTokens:   existing.MaxOutputTokens,
		SupportsStreaming: existing.SupportsStreaming,
		SupportsTools:     existing.SupportsTools,
		SupportsVision:    existing.SupportsVision,
		SupportsReasoning: existing.SupportsReasoning,
		CostInput:         existing.CostInput,
		CostOutput:        existing.CostOutput,
		CostCacheRead:     existing.CostCacheRead,
		CostCacheWrite:    existing.CostCacheWrite,
		IsEnabled:         existing.IsEnabled,
	}
	// Whatever this request states is both the new column value AND a recorded
	// override, so the next model sync refreshes the rest of the row from the
	// catalog without undoing the correction.
	var ov models.ModelOverrides
	if req.DisplayName != nil && *req.DisplayName != "" {
		edit.DisplayName = *req.DisplayName
		ov.DisplayName = req.DisplayName
	}
	if req.ContextTokens != nil && *req.ContextTokens > 0 {
		edit.ContextTokens = *req.ContextTokens
		ov.ContextTokens = req.ContextTokens
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		edit.MaxOutputTokens = *req.MaxOutputTokens
		ov.MaxOutputTokens = req.MaxOutputTokens
	}
	if req.SupportsStreaming != nil {
		edit.SupportsStreaming = *req.SupportsStreaming
		ov.SupportsStreaming = req.SupportsStreaming
	}
	if req.SupportsTools != nil {
		edit.SupportsTools = *req.SupportsTools
		ov.SupportsTools = req.SupportsTools
	}
	if req.SupportsVision != nil {
		edit.SupportsVision = *req.SupportsVision
		ov.SupportsVision = req.SupportsVision
	}
	if req.SupportsReasoning != nil {
		edit.SupportsReasoning = *req.SupportsReasoning
		ov.SupportsReasoning = req.SupportsReasoning
	}
	// Prices accept 0 as a real value ("this model is free"), so unlike the
	// token limits they are recorded whenever the field is present at all.
	if req.CostInput != nil {
		edit.CostInput = *req.CostInput
		ov.CostInput = req.CostInput
	}
	if req.CostOutput != nil {
		edit.CostOutput = *req.CostOutput
		ov.CostOutput = req.CostOutput
	}
	if req.CostCacheRead != nil {
		edit.CostCacheRead = *req.CostCacheRead
		ov.CostCacheRead = req.CostCacheRead
	}
	if req.CostCacheWrite != nil {
		edit.CostCacheWrite = *req.CostCacheWrite
		ov.CostCacheWrite = req.CostCacheWrite
	}
	// is_enabled is deliberately NOT an override: a sync never touches it, so
	// it needs no protection, and recording it would make the eye toggle look
	// like a correction to the catalog.
	if req.IsEnabled != nil {
		edit.IsEnabled = *req.IsEnabled
	}

	if err := models.UpdateModelCapabilities(h.deps.DB.DB, modelPK, edit); err != nil {
		h.deps.serverError(w, "failed to update model", err)
		return
	}
	if req.ResetOverrides {
		if err := models.ClearModelOverrides(h.deps.DB.DB, modelPK, existing.Config); err != nil {
			h.deps.Logger.Error("failed to clear model overrides", "error", err)
		}
	} else if !ov.IsEmpty() {
		if err := models.MergeModelOverrides(h.deps.DB.DB, modelPK, existing.Config, ov); err != nil {
			h.deps.Logger.Error("failed to record model overrides", "error", err)
		}
	}

	if req.Thinking != nil {
		// Read the config back first: the override write above may have changed it,
		// and writing from the stale copy would undo that.
		cur, cErr := models.GetModelByID(h.deps.DB.DB, modelPK)
		if cErr == nil {
			cErr = models.SetModelThinking(h.deps.DB.DB, modelPK, cur.Config, req.Thinking)
		}
		if cErr != nil {
			h.deps.serverError(w, "failed to update model", cErr)
			return
		}
	}

	// Disabling a model that is currently the system default would leave
	// the catalog with an invisible default (selectableWhere filters out
	// disabled rows). Clear the flag so the system falls back to
	// GetDefaultModel's no-default branch instead of pointing at a hidden row.
	if !edit.IsEnabled && existing.IsDefault {
		if _, err := h.deps.DB.ExecWrite("UPDATE llm_models SET is_default = 0 WHERE id = ?", modelPK); err != nil {
			h.deps.Logger.Error("failed to clear default on disabled model", "error", err)
		}
	}

	// An enabled model can now be picked for a project: settle what its listing
	// left unknown before it is.
	if edit.IsEnabled && !existing.IsEnabled {
		probeModelInBackground(h.deps, modelPK)
	}

	fresh, err := models.GetModelByID(h.deps.DB.DB, modelPK)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload model")
		return
	}
	resp := toModelResponse(*fresh)
	if provider, perr := models.GetProviderByID(h.deps.DB.DB, providerID); perr == nil {
		resp.ProbePending = probeCapsFor(provider, fresh, false)
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeleteModel removes a model from a provider.
func (h *ProvidersHandler) DeleteModel(w http.ResponseWriter, r *http.Request) {
	modelID, ok := requireIntParam(w, r, "modelID", "invalid model ID")
	if !ok {
		return
	}

	if n, _ := models.CountProjectsUsingModel(h.deps.DB.DB, modelID); n > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("%d project(s) run this model. Switch them to another model first (Project > Settings), then delete it.", n))
		return
	}
	if err := models.DeleteModel(h.deps.DB.DB, modelID); err != nil {
		h.deps.serverError(w, "failed to delete model", err)
		return
	}

	writeStatus(w, "deleted")
}

// SetDefaultModel marks one chat-kind model as the system-wide default.
// is_default is scoped per-kind: at most one llm-kind model carries the
// flag system-wide, and each image-kind provider has its own default
// (consumed by manage_image_gen via GetDefaultModelForProvider). The
// clear clause below is restricted to kind='llm' so toggling a chat
// default leaves image providers' defaults untouched. Project creation
// pre-selects this model; builds with no model specified fall back to
// it via GetDefaultModel.
func (h *ProvidersHandler) SetDefaultModel(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	modelID, ok := requireIntParam(w, r, "modelID", "invalid model ID")
	if !ok {
		return
	}

	// Promote the chosen model FIRST (and force-enable it: a disabled default
	// would be filtered out of the project-creation catalog, which requires
	// is_enabled = 1). Doing this before demoting the others means a failure of
	// the demote step below never leaves the system with zero defaults. The
	// brief two-defaults window is harmless: GetDefaultModel tolerates it via
	// its is_default DESC, id ASC tie-breaker.
	res, err := h.deps.DB.ExecWrite("UPDATE llm_models SET is_default = 1, is_enabled = 1 WHERE id = ? AND provider_id = ?", modelID, providerID)
	if err != nil {
		h.deps.serverError(w, "failed to set default model", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "model not found for this provider")
		return
	}

	// Demote every OTHER system-wide default.
	if _, err := h.deps.DB.ExecWrite("UPDATE llm_models SET is_default = 0 WHERE is_default = 1 AND id != ? AND provider_id IN (SELECT id FROM providers WHERE kind = 'llm')", modelID); err != nil {
		h.deps.Logger.Error("failed to clear other default models", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update default model")
		return
	}

	writeStatus(w, "default set")
}
