// Handlers for LLM/image/stock provider rows. Split across sibling files:
//   - providers_catalog.go    : Catalog, List, supports-sync probe
//   - providers_crud.go       : Create, Get, Update, Delete, SetDefault
//   - providers_diagnostics.go: TestConfig, Test
//   - providers_models.go     : Models CRUD + SetDefaultModel
//   - providers_sync.go       : SyncModels, SyncAll, internal sync helpers
//
// This file owns only the handler struct, request DTOs, and the cross-file
// helpers (transport-kind discovery).

package admin

import (
	"openfabrica/internal/providercfg"
	"openfabrica/providers/imagegen"
	"openfabrica/providers/stocksearch"
	"openfabrica/providers/websearch"
)

// ProvidersHandler handles LLM provider CRUD and test endpoints.
type ProvidersHandler struct {
	deps *Deps
}

type createProviderRequest struct {
	Name      string  `json:"name"`
	Vendor    string  `json:"vendor"`
	Transport string  `json:"transport"`
	Kind      string  `json:"kind"`
	APIKey    string  `json:"api_key"`
	BaseURL   *string `json:"base_url"`
	ModelsURL string  `json:"models_url"`
	// Config is the provider's behaviour as data (internal/providercfg):
	// credential placement, static headers, the model-listing shape. Optional -
	// omitted means "whatever this transport normally does" - but accepting it
	// here is what makes the claim true that a vendor with an unusual auth
	// header needs no code, only a row. Validated before the insert.
	Config *providercfg.Config `json:"config,omitempty"`
}

type updateProviderRequest struct {
	Name      string              `json:"name"`
	APIKey    string              `json:"api_key,omitempty"`
	BaseURL   *string             `json:"base_url"`
	IsEnabled bool                `json:"is_enabled"`
	Config    *providercfg.Config `json:"config,omitempty"`
}

// kindFromTransport derives the providers.kind discriminator from a
// transport identifier. Each backend self-registers; the admin handler
// asks each registry whether the transport belongs to it. Adding a new
// backend (fal.ai for image-gen, getty for stock, ...) just means
// registering its constructor, no changes here.
func kindFromTransport(t string) string {
	for _, registered := range imagegen.RegisteredTypes() {
		if registered == t {
			return "image"
		}
	}
	for _, registered := range stocksearch.RegisteredTypes() {
		if registered == t {
			return "stock"
		}
	}
	for _, registered := range websearch.RegisteredTypes() {
		if registered == t {
			return "search"
		}
	}
	return "llm"
}

// isNonLLMTransport reports whether a transport is registered in the
// image-gen, stock-search or web-search backends. Used to validate Create
// requests for non-LLM providers that don't live in the llm factory.
func isNonLLMTransport(t string) bool {
	return kindFromTransport(t) != "llm"
}
