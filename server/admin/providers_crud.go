package admin

import (
	"fmt"
	"net/http"
	"strings"

	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/providercfg"
	"openfabrica/internal/safego"
	"openfabrica/providers/llm"
)

// Create creates a new LLM provider.
func (h *ProvidersHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createProviderRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" || req.Transport == "" {
		writeError(w, http.StatusBadRequest, "name and transport are required")
		return
	}
	if !llm.HasTransport(req.Transport) && !isNonLLMTransport(req.Transport) {
		writeError(w, http.StatusBadRequest, "unknown transport: "+req.Transport)
		return
	}
	// Reject a bad config here rather than storing it: providercfg.Parse is
	// deliberately lenient at read time (a mangled blob must not make a
	// provider unreachable), so this is the only place a typo can be caught.
	configJSON, err := marshalProviderConfig(req.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	kind := req.Kind
	if kind == "" {
		kind = kindFromTransport(req.Transport)
	}
	vendor := req.Vendor
	if vendor == "" {
		vendor = slugifyName(req.Name)
	}

	var encryptedKey *string
	if req.APIKey != "" {
		encrypted, err := h.deps.Encryptor.Encrypt(req.APIKey)
		if err != nil {
			h.deps.serverError(w, "failed to encrypt api key", err)
			return
		}
		encryptedKey = &encrypted
	}

	provider, err := models.CreateProvider(h.deps.DB.DB, req.Name, vendor, req.Transport, encryptedKey, req.BaseURL)
	if err != nil {
		// vendor is UNIQUE and every seed.json provider is already a row, so
		// a collision means the admin tried to re-add a built-in. Point them
		// at the existing card instead of returning an opaque 500.
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A provider named \""+req.Name+"\" already exists. Edit the existing one to add a key, or pick a different name.")
			return
		}
		h.deps.serverError(w, "failed to create provider", err)
		return
	}

	if kind != "llm" {
		if err := models.SetProviderKind(h.deps.DB.DB, provider.ID, kind); err != nil {
			h.deps.serverError(w, "failed to set provider kind", err)
			return
		}
		provider.Kind = kind
	}
	if req.ModelsURL != "" {
		if _, err := h.deps.DB.ExecWrite("UPDATE providers SET models_url = ? WHERE id = ?", req.ModelsURL, provider.ID); err != nil {
			h.deps.Logger.Error("failed to set models_url", "error", err)
		}
		provider.ModelsURL = req.ModelsURL
	}
	if configJSON != "" {
		if _, err := h.deps.DB.ExecWrite("UPDATE providers SET config = ? WHERE id = ?", configJSON, provider.ID); err != nil {
			h.deps.Logger.Error("failed to set provider config", "error", err)
		}
		provider.Config = configJSON
	}

	if kind == "llm" && req.APIKey != "" {
		if liveProvider, lpErr := llm.NewProvider(provider.Transport, provider.ProviderOptions(req.APIKey, h.deps.LLMHTTPClient)); lpErr == nil {
			h.deps.LLMRegistry.Register(provider.Name, liveProvider)
		}
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProviderAdded, 0, map[string]any{
			"provider_id": provider.ID,
			"name":        provider.Name,
		}))
	}

	writeJSON(w, http.StatusCreated, provider)
}

// Get returns a single provider by ID.
func (h *ProvidersHandler) Get(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	provider, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	writeJSON(w, http.StatusOK, provider)
}

// Update updates an existing provider.
func (h *ProvidersHandler) Update(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	var req updateProviderRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Get the existing provider for re-registration.
	existing, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	// If a new API key is provided, encrypt it; otherwise keep the existing one.
	var encryptedKey *string
	plainAPIKey := req.APIKey
	if plainAPIKey != "" {
		encrypted, err := h.deps.Encryptor.Encrypt(plainAPIKey)
		if err != nil {
			h.deps.serverError(w, "failed to encrypt api key", err)
			return
		}
		encryptedKey = &encrypted
	} else {
		encryptedKey = existing.APIKeyEncrypted
	}

	if err := models.UpdateProvider(h.deps.DB.DB, providerID, req.Name, encryptedKey, req.BaseURL, req.IsEnabled); err != nil {
		h.deps.serverError(w, "failed to update provider", err)
		return
	}
	if req.Config != nil {
		configJSON, cErr := marshalProviderConfig(req.Config)
		if cErr != nil {
			writeError(w, http.StatusBadRequest, cErr.Error())
			return
		}
		if _, cErr := h.deps.DB.ExecWrite("UPDATE providers SET config = ? WHERE id = ?", configJSON, providerID); cErr != nil {
			h.deps.serverError(w, "failed to update provider config", cErr)
			return
		}
	}

	provider, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provider updated but failed to reload")
		return
	}

	// Re-register in live LLM registry with updated credentials. Image
	// providers don't belong in the LLM registry, they're constructed
	// on demand by manage_image_gen, so skip the call for kind != 'llm'.
	var apiKey string
	if plainAPIKey != "" {
		apiKey = plainAPIKey
	} else if existing.APIKeyEncrypted != nil {
		if decrypted, decErr := h.deps.Encryptor.Decrypt(*existing.APIKeyEncrypted); decErr == nil {
			apiKey = decrypted
		}
	}
	if existing.Kind == "llm" && !provider.IsEnabled {
		// A disabled provider must stop serving: the registry would otherwise keep
		// the instance it holds and callers resolving by name would still reach it.
		h.deps.LLMRegistry.Unregister(existing.Name)
		llm.ResetHealth(existing.Name)
	} else if existing.Kind == "llm" && (apiKey != "" || !provider.RequiresAPIKey) {
		// A keyless local provider (Ollama, llama.cpp) re-registers too, so a config
		// change reaches the live client without a restart.
		// Start from the stored row so models_url and config come along, then
		// overlay whatever this request is changing.
		opts := provider.ProviderOptions(apiKey, h.deps.LLMHTTPClient)
		if req.BaseURL != nil {
			opts.BaseURL = *req.BaseURL
		}
		if req.Name != "" {
			opts.Name = req.Name
		}
		name := opts.Name
		if liveProvider, lpErr := llm.NewProvider(provider.Transport, opts); lpErr == nil {
			h.deps.LLMRegistry.Register(name, liveProvider)

			// Auto-sync models on first-time key entry. The trigger is "the
			// provider had no key before and now has one and has no models
			// yet", admins entering a key in setup expect a populated picker
			// without an extra click. Failure is non-fatal: we just log and
			// let them invoke /sync-models manually.
			firstKey := plainAPIKey != "" && (existing.APIKeyEncrypted == nil || *existing.APIKeyEncrypted == "")
			if firstKey {
				if existingModels, _ := models.ListModelsByProvider(h.deps.DB.DB, provider.ID); len(existingModels) == 0 {
					safego.Go("provider-sync", func() { h.runSync(provider.ID, liveProvider) })
				}
			}
		}
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProviderUpdated, 0, map[string]any{
			"provider_id": provider.ID,
			"name":        provider.Name,
		}))
	}

	writeJSON(w, http.StatusOK, provider)
}

// Delete removes a provider.
func (h *ProvidersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	existing, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	// A project points at a model by id with no foreign key, so deleting under it
	// would strand the project on a model that no longer exists.
	if n, _ := models.CountProjectsUsingProvider(h.deps.DB.DB, providerID); n > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("%d project(s) use a model from this provider. Switch them to another model first (Project > Settings), then delete it.", n))
		return
	}
	if err := models.DeleteProvider(h.deps.DB.DB, providerID); err != nil {
		h.deps.serverError(w, "failed to delete provider", err)
		return
	}
	// Stop the live instance too, or it keeps answering chat and endpoints.
	h.deps.LLMRegistry.Unregister(existing.Name)

	writeStatus(w, "deleted")
}

// SetDefault marks the provider as the per-kind default. Used by
// image/stock kinds where there is no useful model-level default (stock
// has a single synthetic model; image's per-provider default already
// lives on the model row). The image/stock tool resolvers prefer the
// is_default=1 row when the LLM calls without a provider name.
func (h *ProvidersHandler) SetDefault(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}
	if err := models.SetProviderAsDefaultForKind(h.deps.DB.DB, providerID); err != nil {
		h.deps.serverError(w, "failed to set default provider", err)
		return
	}
	writeStatus(w, "default set")
}

// marshalProviderConfig validates a caller-supplied provider config and renders
// it for storage. Returns "" when cfg is nil, meaning "leave the column alone".
func marshalProviderConfig(cfg *providercfg.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	return cfg.Marshal()
}
