package admin

import (
	"context"
	"net/http"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/providers/llm"
)

// TestConfig probes an arbitrary provider config without persisting anything.
// Used by the Add/Edit modal so a user can verify their API key before
// committing the row. Symmetric with Test (which probes a stored provider)
// but takes the credentials in the request body. Image and stock providers
// fall back to "configured but not exercised", same rationale as Test.
func (h *ProvidersHandler) TestConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Transport string `json:"transport"`
		APIKey    string `json:"api_key"`
		BaseURL   string `json:"base_url"`
		ModelID   string `json:"model_id"`
		// ProviderID names a stored provider whose key to reuse when APIKey is
		// blank (the Edit modal never shows the stored key). Optional.
		ProviderID int `json:"provider_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Transport == "" {
		writeError(w, http.StatusBadRequest, "transport is required")
		return
	}

	// A blank key on an existing provider means "test the key already saved".
	// The stored key is only reused for that provider's own base URL, so a test
	// request can never carry a saved key to a host the admin just typed in.
	if req.APIKey == "" && req.ProviderID > 0 {
		stored, err := models.GetProviderByID(h.deps.DB.DB, req.ProviderID)
		if err != nil || stored.Transport != req.Transport {
			writeError(w, http.StatusBadRequest, "stored provider does not match this transport")
			return
		}
		storedURL := ""
		if stored.BaseURL != nil {
			storedURL = *stored.BaseURL
		}
		if req.BaseURL != "" && req.BaseURL != storedURL {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   msgcat.Localize(r, "the base URL changed, so the saved key will not be sent to it. Paste the key to test this URL"),
			})
			return
		}
		if stored.APIKeyEncrypted != nil && *stored.APIKeyEncrypted != "" {
			key, decErr := h.deps.Encryptor.Decrypt(*stored.APIKeyEncrypted)
			if decErr != nil {
				writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": msgcat.Localize(r, "failed to decrypt the saved API key")})
				return
			}
			req.APIKey = key
		}
		if req.BaseURL == "" {
			req.BaseURL = storedURL
		}
		// With no explicit probe model, use one of this provider's own.
		if req.ModelID == "" {
			ms, _ := models.ListModelsByProvider(h.deps.DB.DB, stored.ID)
			for _, m := range ms {
				if m.IsDefault {
					req.ModelID = m.ModelID
					break
				}
			}
			if req.ModelID == "" && len(ms) > 0 {
				req.ModelID = ms[0].ModelID
			}
		}
	}

	kind := kindFromTransport(req.Transport)
	if kind == "image" || kind == "stock" || kind == "search" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":   true,
			"exercised": false,
			"info":      "credentials accepted; this provider type can only be verified by a real generation or search",
		})
		return
	}

	// Build an ephemeral chat provider and ping it. Picks a probe model in
	// this order: explicit model_id from the request -> first model of any
	// existing provider sharing this provider_type -> bail out asking for
	// model_id. Centralising the fallback here keeps the modal frontend
	// from having to know which model to probe with.
	pingModel := req.ModelID
	if pingModel == "" {
		existing, _ := models.ListProviders(h.deps.DB.DB)
		for _, p := range existing {
			if p.Transport != req.Transport {
				continue
			}
			ms, _ := models.ListModelsByProvider(h.deps.DB.DB, p.ID)
			for _, m := range ms {
				if m.IsDefault {
					pingModel = m.ModelID
					break
				}
			}
			if pingModel == "" && len(ms) > 0 {
				pingModel = ms[0].ModelID
			}
			if pingModel != "" {
				break
			}
		}
	}
	if pingModel == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   msgcat.Localize(r, "no probe model available, pick a model in the form first"),
		})
		return
	}

	// Unsaved form values: there is no row to read config from, so this probe
	// exercises the transport's own defaults. A provider whose auth or listing
	// shape lives entirely in config will therefore be tested with defaults,
	// which is the honest answer to "do these credentials reach that URL".
	liveProvider, err := llm.NewProvider(req.Transport, llm.Options{
		Name:       "__test__",
		APIKey:     req.APIKey,
		BaseURL:    req.BaseURL,
		HTTPClient: h.deps.LLMHTTPClient,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := liveProvider.Ping(ctx, pingModel); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// Test tests connectivity to a provider by calling its Ping method.
func (h *ProvidersHandler) Test(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	dbProvider, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	// Non-LLM providers (image-gen, stock-search) don't have a cheap Ping
	// path, the closest equivalent is a real generation/search, which
	// costs money or rate-limit budget. Surface a clear "OK to configure,
	// but won't be exercised here" status instead of pretending to test
	// something that won't run.
	if dbProvider.Kind == "image" || dbProvider.Kind == "stock" || dbProvider.Kind == "search" {
		if !dbProvider.IsAvailable() {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   msgcat.Localize(r, "provider unavailable (disabled or missing key)"),
			})
			return
		}
		var info string
		switch dbProvider.Kind {
		case "image":
			info = "image provider configured; trigger a real generation via manage_image_gen to verify connectivity"
		case "stock":
			info = "stock-search provider configured; trigger a real search via manage_stockphotos to verify connectivity"
		case "search":
			info = "web-search provider configured; run a real query via manage_web_search to verify connectivity"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":   true,
			"exercised": false,
			"info":      info,
		})
		return
	}

	// Try to get the provider from the live registry.
	liveProvider, regErr := h.deps.LLMRegistry.Get(dbProvider.Name)
	if regErr != nil {
		// Provider not in live registry - create on-the-fly from DB credentials.
		if dbProvider.APIKeyEncrypted == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   msgcat.Localize(r, "no API key configured for this provider"),
			})
			return
		}
		apiKey, decErr := h.deps.Encryptor.Decrypt(*dbProvider.APIKeyEncrypted)
		if decErr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   msgcat.Localize(r, "failed to decrypt API key"),
			})
			return
		}
		var lpErr error
		liveProvider, lpErr = llm.NewProvider(dbProvider.Transport, dbProvider.ProviderOptions(apiKey, h.deps.LLMHTTPClient))
		if lpErr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   lpErr.Error(),
			})
			return
		}
		// Cache it in the registry for future use.
		h.deps.LLMRegistry.Register(dbProvider.Name, liveProvider)
	}

	// Get the default model for this provider from DB
	dbModels, _ := models.ListModelsByProvider(h.deps.DB.DB, providerID)
	pingModel := ""
	for _, m := range dbModels {
		if m.IsDefault {
			pingModel = m.ModelID
			break
		}
	}
	if pingModel == "" && len(dbModels) > 0 {
		pingModel = dbModels[0].ModelID
	}
	if pingModel == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   msgcat.Localize(r, "no models configured for this provider"),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := liveProvider.Ping(ctx, pingModel); err != nil {
		llm.RecordProviderCall(dbProvider.Name, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   err.Error(),
		})
		return
	}
	llm.RecordProviderCall(dbProvider.Name, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
	})
}
