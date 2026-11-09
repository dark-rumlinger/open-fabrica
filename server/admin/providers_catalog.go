package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"openfabrica/db/models"
	"openfabrica/providers/imagegen"
	"openfabrica/providers/llm"
	"openfabrica/providers/stocksearch"
	"openfabrica/providers/websearch"
)

// Catalog returns providers that can serve requests right now, each
// bundled with the models that are individually enabled. Used by the
// project creation modal as the source of truth for "what can I pick".
//
// "Selectable" is computed by db/models.ListSelectableModels, provider
// enabled AND model enabled AND (no key required OR key configured).
// The setup wizard shares the same predicate so the two endpoints stay
// aligned on what counts as available.
func (h *ProvidersHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	rows, err := models.ListSelectableModels(h.deps.DB.DB)
	if err != nil {
		h.deps.Logger.Error("failed to list selectable models", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}

	// Group rows by provider, preserving the SQL ORDER BY (provider name,
	// then model display name). The frontend's existing dropdown shape is
	// "provider with models[]"; keep that contract.
	byProvider := map[int]*setupProviderResponse{}
	order := []int{}
	for _, row := range rows {
		entry, ok := byProvider[row.Provider.ID]
		if !ok {
			entry = &setupProviderResponse{
				ID:             row.Provider.ID,
				Name:           row.Provider.Name,
				Vendor:         row.Provider.Vendor,
				Transport:      row.Provider.Transport,
				Plan:           row.Provider.Plan,
				IsEnabled:      row.Provider.IsEnabled,
				RequiresAPIKey: row.Provider.RequiresAPIKey,
				HasAPIKey:      row.Provider.APIKeyEncrypted != nil && *row.Provider.APIKeyEncrypted != "",
				IsAvailable:    row.Provider.IsAvailable(),
				Models:         []setupModelResponse{},
			}
			byProvider[row.Provider.ID] = entry
			order = append(order, row.Provider.ID)
		}
		entry.Models = append(entry.Models, setupModelResponse{
			ID:             row.Model.ID,
			ModelID:        row.Model.ModelID,
			DisplayName:    row.Model.DisplayName,
			IsDefault:      row.Model.IsDefault,
			IsEnabled:      row.Model.IsEnabled,
			SupportsTools:  row.Model.SupportsTools,
			SupportsVision: row.Model.SupportsVision,
		})
	}

	result := make([]setupProviderResponse, 0, len(order))
	for _, id := range order {
		result = append(result, *byProvider[id])
	}
	writeJSON(w, http.StatusOK, result)
}

// providerListResponse mirrors LLMProvider but adds the derived has_api_key
// and is_available booleans the admin UI uses for the status pill. Sending
// these from the server keeps the client from re-implementing the
// availability rule and avoids exposing api_key_encrypted directly.
type providerListResponse struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	Vendor         string  `json:"vendor"`
	Transport      string  `json:"transport"`
	Kind           string  `json:"kind"`
	Plan           string  `json:"plan"`
	BaseURL        *string `json:"base_url"`
	ModelsURL      string  `json:"models_url"`
	IsEnabled      bool    `json:"is_enabled"`
	RequiresAPIKey bool    `json:"requires_api_key"`
	HasAPIKey      bool    `json:"has_api_key"`
	IsAvailable    bool    `json:"is_available"`
	IsDefault      bool    `json:"is_default"`
	SignupURL      string  `json:"signup_url"`
	// SupportsSync exposes whether SyncModels can be invoked. True when the
	// transport's live provider implements llm.ModelLister AND the row has
	// either a key configured or is keyless.
	SupportsSync bool `json:"supports_sync"`
	// ProbesCaps marks an LLM provider whose listing does not report reasoning or
	// tool use, so its card offers "check capabilities".
	ProbesCaps bool `json:"probes_caps"`
	// ShowBaseURL / BaseURLRequired drive the Edit modal's Base URL field.
	// Derived from the transport's metadata so the frontend doesn't carry
	// its own copy of per-transport form behavior.
	ShowBaseURL     bool `json:"show_base_url"`
	BaseURLRequired bool `json:"base_url_required"`
	// Config is the row's providercfg blob, sent as an object so the Edit
	// modal can round-trip it. Emitted even when empty ({}), so the advanced
	// editor always has something valid to open with.
	Config json.RawMessage `json:"config"`
	// Health is what the last real calls to this provider returned, since the
	// server started. Absent until a call has been made.
	Health *llm.ProviderHealth `json:"health,omitempty"`
}

// configJSON renders a provider's stored config for the list response. A blank
// or malformed blob comes back as {} rather than null or an error: config is
// advisory decoration on a row that is otherwise usable, and the provider list
// must not fail to render because one row's options JSON got mangled.
func configJSON(raw string) json.RawMessage {
	if !json.Valid([]byte(raw)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}

// List returns all providers (including disabled / keyless / key-missing
// ones) so the admin can manage every row. Selectability is conveyed via
// is_available rather than by filtering rows out, the UI shows status
// pills so the admin knows *why* a provider is or isn't usable.
func (h *ProvidersHandler) List(w http.ResponseWriter, r *http.Request) {
	kindFilter := r.URL.Query().Get("kind")
	var providers []models.LLMProvider
	var err error
	if kindFilter != "" {
		providers, err = models.ListProvidersByKind(h.deps.DB.DB, kindFilter)
	} else {
		providers, err = models.ListProviders(h.deps.DB.DB)
	}
	if err != nil {
		h.deps.serverError(w, "failed to list providers", err)
		return
	}

	out := make([]providerListResponse, 0, len(providers))
	for _, p := range providers {
		hasKey := p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != ""
		meta := transportMeta[strings.ToLower(p.Transport)]
		var health *llm.ProviderHealth
		if ph, ok := llm.HealthOf(p.Name); ok {
			health = &ph
		}
		out = append(out, providerListResponse{
			ID:              p.ID,
			Name:            p.Name,
			Vendor:          p.Vendor,
			Transport:       p.Transport,
			Kind:            p.Kind,
			Plan:            p.Plan,
			BaseURL:         p.BaseURL,
			ModelsURL:       p.ModelsURL,
			IsEnabled:       p.IsEnabled,
			RequiresAPIKey:  p.RequiresAPIKey,
			HasAPIKey:       hasKey,
			IsAvailable:     p.IsAvailable(),
			IsDefault:       p.IsDefault,
			SignupURL:       p.SignupURL,
			SupportsSync:    h.providerSupportsSync(p),
			ProbesCaps:      p.Kind == "llm" && !listingReportsCaps(&p),
			ShowBaseURL:     meta.ShowBaseURL,
			BaseURLRequired: meta.BaseURLRequired,
			Config:          configJSON(p.Config),
			Health:          health,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// providerSupportsSync returns true when this provider's transport is
// known to implement llm.ModelLister. We probe by constructing an ephemeral
// live provider with a placeholder key, so the answer reflects the
// transport's capability, not the row's runtime state.
//
// Deliberately NOT gated on models_url being set: a listing URL is derived
// from the base root whenever the row leaves it empty, so most providers
// carry no explicit models_url and an empty one means "derive it", not
// "cannot sync".
func (h *ProvidersHandler) providerSupportsSync(p models.LLMProvider) bool {
	switch p.Kind {
	case "llm":
		probe, err := llm.NewProvider(p.Transport, p.ProviderOptions("probe", h.deps.LLMHTTPClient))
		if err != nil {
			return false
		}
		_, ok := probe.(llm.ModelLister)
		return ok
	case "image":
		probe, err := imagegen.NewProvider(p.Transport, p.ProviderOptions("probe", h.deps.LLMHTTPClient))
		if err != nil {
			return false
		}
		_, ok := probe.(imagegen.ModelLister)
		return ok
	}
	return false
}

// transportMetaEntry is the display + form metadata for one transport.
// The set of selectable transports is owned by the backend registries
// (llm/imagegen/stocksearch/websearch RegisteredTypes); this map only
// enriches them with the friendly label and Base URL / API-key behavior
// the Add and Edit modals need. A transport with no entry still appears,
// it just falls back to its bare identifier as the label.
type transportMetaEntry struct {
	Label            string
	DefaultBaseURL   string
	DefaultModelsURL string
	RequiresAPIKey   bool
	ShowBaseURL      bool
	BaseURLRequired  bool
}

// transportMeta keys on the transport identifier (matches the factory
// registries). Keeping it here gives provider-type knowledge a single
// home on the server, which the admin frontend reads at runtime.
var transportMeta = map[string]transportMetaEntry{
	"anthropic": {
		Label:            "Anthropic",
		DefaultBaseURL:   "https://api.anthropic.com/v1",
		DefaultModelsURL: "https://api.anthropic.com/v1/models",
		RequiresAPIKey:   true,
		ShowBaseURL:      true,
	},
	"openai-chat": {
		// The catch-all. Every vendor that copied /v1/chat/completions is
		// this transport plus a base URL, which is why the label names the
		// protocol rather than a vendor.
		Label:            "OpenAI-compatible chat API",
		DefaultBaseURL:   "https://api.openai.com/v1",
		DefaultModelsURL: "https://api.openai.com/v1/models",
		RequiresAPIKey:   true,
		ShowBaseURL:      true,
		BaseURLRequired:  true,
	},
	"ollama": {
		Label:            "Ollama (local models)",
		DefaultBaseURL:   "http://localhost:11434/v1",
		DefaultModelsURL: "http://localhost:11434/api/tags",
		ShowBaseURL:      true,
		BaseURLRequired:  true,
	},
	"openai-image": {
		Label:          "OpenAI Images",
		DefaultBaseURL: "https://api.openai.com/v1",
		RequiresAPIKey: true,
		ShowBaseURL:    true,
	},
	"automatic1111": {
		Label:           "Automatic1111 (local Stable Diffusion)",
		DefaultBaseURL:  "http://127.0.0.1:7860",
		ShowBaseURL:     true,
		BaseURLRequired: true,
	},
	"pexels": {
		Label:          "Pexels (stock photos)",
		DefaultBaseURL: "https://api.pexels.com/v1",
		RequiresAPIKey: true,
	},
	"unsplash": {
		Label:          "Unsplash (stock photos)",
		DefaultBaseURL: "https://api.unsplash.com",
		RequiresAPIKey: true,
	},
	"pixabay": {
		Label:          "Pixabay (stock photos)",
		DefaultBaseURL: "https://pixabay.com/api",
		RequiresAPIKey: true,
	},
	"tavily": {
		Label:          "Tavily (web search)",
		DefaultBaseURL: "https://api.tavily.com",
		RequiresAPIKey: true,
	},
	"brave": {
		Label:          "Brave Search (web search)",
		DefaultBaseURL: "https://api.search.brave.com/res/v1",
		RequiresAPIKey: true,
	},
}

// providerTypeInfo is one selectable provider type returned by Transports.
type providerTypeInfo struct {
	Transport        string `json:"transport"`
	Kind             string `json:"kind"`
	Label            string `json:"label"`
	DefaultBaseURL   string `json:"default_base_url"`
	DefaultModelsURL string `json:"default_models_url"`
	RequiresAPIKey   bool   `json:"requires_api_key"`
	ShowBaseURL      bool   `json:"show_base_url"`
	BaseURLRequired  bool   `json:"base_url_required"`
}

// Transports returns the provider types the build actually supports,
// gathered from every backend registry and enriched from transportMeta.
// The Add Provider modal renders its dropdown from this, so the list lives
// only on the server. Optional ?kind= filters to one kind.
func (h *ProvidersHandler) Transports(w http.ResponseWriter, r *http.Request) {
	kindFilter := r.URL.Query().Get("kind")

	all := llm.RegisteredTypes()
	all = append(all, imagegen.RegisteredTypes()...)
	all = append(all, stocksearch.RegisteredTypes()...)
	all = append(all, websearch.RegisteredTypes()...)

	seen := map[string]bool{}
	out := make([]providerTypeInfo, 0, len(all))
	for _, t := range all {
		if seen[t] {
			continue
		}
		seen[t] = true
		kind := kindFromTransport(t)
		if kindFilter != "" && kind != kindFilter {
			continue
		}
		meta := transportMeta[t]
		label := meta.Label
		if label == "" {
			label = t
		}
		out = append(out, providerTypeInfo{
			Transport:        t,
			Kind:             kind,
			Label:            label,
			DefaultBaseURL:   meta.DefaultBaseURL,
			DefaultModelsURL: meta.DefaultModelsURL,
			RequiresAPIKey:   meta.RequiresAPIKey,
			ShowBaseURL:      meta.ShowBaseURL,
			BaseURLRequired:  meta.BaseURLRequired,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
