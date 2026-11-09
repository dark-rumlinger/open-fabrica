package admin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/safego"
	"openfabrica/providers/imagegen"
	"openfabrica/providers/llm"
)

// SyncModels asks the live provider for its current model catalog and
// upserts the result into llm_models. Routes by kind: LLM rows use
// llm.ModelLister, image rows use imagegen.ModelLister. Stock rows have
// a single synthetic model so they're not sync-eligible. Newly discovered
// models are inserted disabled by default so a large catalog (OpenRouter,
// A1111 with dozens of checkpoints) doesn't flood project pickers.
func (h *ProvidersHandler) SyncModels(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireIntParam(w, r, "providerID", "invalid provider ID")
	if !ok {
		return
	}

	dbProvider, err := models.GetProviderByID(h.deps.DB.DB, providerID)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var rows []models.SyncedModel
	switch dbProvider.Kind {
	case "llm":
		var discovered []llm.ModelInfo
		discovered, err = h.syncLLMModels(ctx, dbProvider)
		if err != nil {
			break
		}
		stats, serr := syncLLMCatalog(h.deps, providerID, discovered, false)
		if serr != nil {
			h.deps.Logger.Error("model upsert failed", "provider_id", providerID, "error", serr)
			writeError(w, http.StatusInternalServerError, "failed to write models: "+serr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"discovered": len(discovered),
			"added":      stats.Added,
			"updated":    stats.Updated,
		})
		return
	case "image":
		rows, err = h.syncImageModels(ctx, dbProvider)
	default:
		writeError(w, http.StatusBadRequest, "this provider does not support model discovery")
		return
	}
	if err != nil {
		// Errors carry their own HTTP-shaped reason via httpStatusError;
		// fall back to 502 for transport-level failures.
		if se, ok := err.(*httpStatusError); ok {
			writeError(w, se.status, se.msg)
			return
		}
		h.deps.Logger.Error("model sync failed", "provider_id", providerID, "error", err)
		writeError(w, http.StatusBadGateway, "model sync failed: "+err.Error())
		return
	}

	stats, err := models.SyncProviderModels(h.deps.DB.DB, providerID, rows, false)
	if err != nil {
		h.deps.Logger.Error("model upsert failed", "provider_id", providerID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to write models: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"discovered": len(rows),
		"added":      stats.Added,
		"updated":    stats.Updated,
	})
}

// httpStatusError lets the kind-specific sync helpers signal a specific
// HTTP status (400 for "no key", 400 for "not supported") without
// flattening every failure to 502. The outer handler unwraps it before
// writing the response.
type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return e.msg }

func (h *ProvidersHandler) syncLLMModels(ctx context.Context, dbProvider *models.LLMProvider) ([]llm.ModelInfo, error) {
	liveProvider, regErr := h.deps.LLMRegistry.Get(dbProvider.Name)
	if regErr != nil {
		if dbProvider.APIKeyEncrypted == nil {
			return nil, &httpStatusError{http.StatusBadRequest, msgcat.Mark("no API key configured for this provider")}
		}
		apiKey, decErr := h.deps.Encryptor.Decrypt(*dbProvider.APIKeyEncrypted)
		if decErr != nil {
			return nil, &httpStatusError{http.StatusInternalServerError, msgcat.Mark("failed to decrypt API key")}
		}
		var lpErr error
		liveProvider, lpErr = llm.NewProvider(dbProvider.Transport, dbProvider.ProviderOptions(apiKey, h.deps.LLMHTTPClient))
		if lpErr != nil {
			return nil, &httpStatusError{http.StatusInternalServerError, lpErr.Error()}
		}
		h.deps.LLMRegistry.Register(dbProvider.Name, liveProvider)
	}

	lister, ok := liveProvider.(llm.ModelLister)
	if !ok {
		return nil, &httpStatusError{http.StatusBadRequest, msgcat.Mark("this provider does not support model discovery")}
	}

	discovered, err := lister.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return discovered, nil
}

// syncImageModels builds an ephemeral imagegen.Provider and asks it to
// list its catalog. Image providers don't live in a long-lived registry
// (unlike LLM), so we construct one per sync call. Key resolution mirrors
// the LLM path: decrypt the stored key, or accept the empty string for
// keyless backends (Automatic1111).
func (h *ProvidersHandler) syncImageModels(ctx context.Context, dbProvider *models.LLMProvider) ([]models.SyncedModel, error) {
	apiKey := ""
	if dbProvider.APIKeyEncrypted != nil && *dbProvider.APIKeyEncrypted != "" {
		decKey, decErr := h.deps.Encryptor.Decrypt(*dbProvider.APIKeyEncrypted)
		if decErr != nil {
			return nil, &httpStatusError{http.StatusInternalServerError, msgcat.Mark("failed to decrypt API key")}
		}
		apiKey = decKey
	} else if dbProvider.RequiresAPIKey {
		return nil, &httpStatusError{http.StatusBadRequest, msgcat.Mark("no API key configured for this provider")}
	}

	live, err := imagegen.NewProvider(dbProvider.Transport, dbProvider.ProviderOptions(apiKey, h.deps.LLMHTTPClient))
	if err != nil {
		return nil, &httpStatusError{http.StatusInternalServerError, err.Error()}
	}
	lister, ok := live.(imagegen.ModelLister)
	if !ok {
		return nil, &httpStatusError{http.StatusBadRequest, msgcat.Mark("this image provider does not support model discovery")}
	}

	discovered, err := lister.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]models.SyncedModel, 0, len(discovered))
	for _, m := range discovered {
		rows = append(rows, models.SyncedModel{ID: m.ID, DisplayName: m.DisplayName})
	}
	return rows, nil
}

// SyncAll fans out a sync against every enabled, available LLM provider
// whose transport implements llm.ModelLister. Errors are collected per
// provider so a single failure doesn't abort the batch; the response is a
// per-provider report the admin UI can render as a toast.
func (h *ProvidersHandler) SyncAll(w http.ResponseWriter, r *http.Request) {
	providers, err := models.ListProviders(h.deps.DB.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}
	type result struct {
		ProviderID int    `json:"provider_id"`
		Name       string `json:"name"`
		Discovered int    `json:"discovered"`
		Added      int    `json:"added"`
		Updated    int    `json:"updated"`
		Error      string `json:"error,omitempty"`
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
	)
	for _, p := range providers {
		if p.Kind != "llm" || !p.IsAvailable() {
			continue
		}
		if !h.providerSupportsSync(p) {
			continue
		}
		wg.Add(1)
		go func(p models.LLMProvider) {
			defer wg.Done()
			defer safego.Recover("provider-model-list")
			res := result{ProviderID: p.ID, Name: p.Name}
			live, err := h.getOrBuildLiveProvider(p)
			if err != nil {
				res.Error = err.Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}
			lister, ok := live.(llm.ModelLister)
			if !ok {
				res.Error = "transport has no ListModels"
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			discovered, err := lister.ListModels(ctx)
			if err != nil {
				res.Error = err.Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}
			stats, err := syncLLMCatalog(h.deps, p.ID, discovered, false)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Discovered = len(discovered)
				res.Added = stats.Added
				res.Updated = stats.Updated
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"providers": results})
}

// getOrBuildLiveProvider returns the registered live provider for p, or
// constructs one from the stored credentials. Used by SyncAll and the
// auto-sync path; centralised here so credential decryption / fallback
// stays consistent.
func (h *ProvidersHandler) getOrBuildLiveProvider(p models.LLMProvider) (llm.Provider, error) {
	return liveLLMProvider(h.deps, p)
}

// runSync is the background variant invoked when an admin enters an API
// key for a provider with no models yet. Best-effort; failures are logged
// and surfaced via the regular /sync-models endpoint on retry.
func (h *ProvidersHandler) runSync(providerID int, live llm.Provider) {
	lister, ok := live.(llm.ModelLister)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	discovered, err := lister.ListModels(ctx)
	if err != nil {
		h.deps.Logger.Warn("auto-sync failed", "provider_id", providerID, "error", err)
		return
	}
	if _, err := syncLLMCatalog(h.deps, providerID, discovered, false); err != nil {
		h.deps.Logger.Warn("auto-sync write failed", "provider_id", providerID, "error", err)
	}
}
