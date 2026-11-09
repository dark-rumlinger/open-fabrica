package content

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/tools"

	"openfabrica/db/models"
	"openfabrica/providers/websearch"
)

// manage_web_search, real-time web search for the build/live model via the
// configured search providers (Tavily, Brave). Provider rows live in
// providers with kind='search'; supported types are registered in the
// websearch package. API keys come from the `auth.env` declared in
// seed.json (env var, first boot only) or the encrypted column on the
// provider row. Mirrors the resolve+construct shape of manage_stockphotos.

func webSearchQuery(ctx *tools.ToolContext, query string, count int, topic, freshness, providerName string) (*tools.Result, error) {
	if strings.TrimSpace(query) == "" {
		return tools.Fail("query is required"), nil
	}

	provider, gen, err := buildSearchProvider(ctx, providerName)
	if err != nil {
		return tools.FailErr(err), nil
	}

	resp, err := gen.Search(ctx.Context(), websearch.SearchRequest{
		Query:     query,
		Count:     clampCount(count),
		Topic:     topic,
		Freshness: freshness,
	})
	if err != nil {
		return tools.FailErr(err), nil
	}

	out := map[string]any{
		"provider": provider.Name,
		"query":    query,
		"count":    len(resp.Results),
		"results":  resp.Results,
	}
	if resp.Answer != "" {
		out["answer"] = resp.Answer
	}
	if len(resp.Results) == 0 {
		out["note"] = "no results; try a broader query or drop the freshness window"
	}
	return tools.Ok(out), nil
}

func webSearchListProviders(ctx *tools.ToolContext) (*tools.Result, error) {
	db := ctx.GlobalDBOrProject()
	providers, err := models.ListProvidersByKind(db, "search")
	if err != nil {
		return tools.FailErr(err), nil
	}
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, map[string]any{
			"name":         p.Name,
			"transport":    p.Transport,
			"is_enabled":   p.IsEnabled,
			"is_available": p.IsAvailable(),
		})
	}
	return tools.Ok(map[string]any{
		"providers":        out,
		"registered_types": websearch.RegisteredTypes(),
	}), nil
}

// buildSearchProvider resolves a search provider row from the global DB and
// constructs a ready-to-use websearch.Provider. Mirrors buildStockProvider.
func buildSearchProvider(ctx *tools.ToolContext, name string) (*models.LLMProvider, websearch.Provider, error) {
	provider, apiKey, err := resolveSearchProviderRow(ctx, name)
	if err != nil {
		return nil, nil, err
	}

	gen, err := websearch.NewProvider(provider.Transport, provider.ProviderOptions(apiKey, nil))
	if err != nil {
		return nil, nil, fmt.Errorf("constructing search provider: %s. Registered types: %v", err.Error(), websearch.RegisteredTypes())
	}
	return provider, gen, nil
}

// resolveSearchProviderRow returns the LLMProvider row to use plus the
// resolved API key. Key resolution prefers the named provider, else the
// admin-flagged default for kind='search', else the first available row.
func resolveSearchProviderRow(ctx *tools.ToolContext, name string) (*models.LLMProvider, string, error) {
	db := ctx.GlobalDBOrProject()

	var p *models.LLMProvider
	if name != "" {
		var err error
		p, err = models.GetProviderByName(db, name)
		if err == sql.ErrNoRows {
			return nil, "", fmt.Errorf("no provider named %q, call manage_web_search list_providers to see configured backends", name)
		}
		if err != nil {
			return nil, "", fmt.Errorf("looking up provider: %w", err)
		}
		if p.Kind != "search" {
			return nil, "", fmt.Errorf("provider %q is kind=%q, not 'search'", name, p.Kind)
		}
	} else {
		if def, err := models.GetDefaultProviderByKind(db, "search"); err == nil && def.IsAvailable() {
			p = def
		} else {
			providers, err := models.ListProvidersByKind(db, "search")
			if err != nil {
				return nil, "", fmt.Errorf("listing search providers: %w", err)
			}
			for i := range providers {
				if providers[i].IsAvailable() {
					p = &providers[i]
					break
				}
			}
		}
		if p == nil {
			return nil, "", fmt.Errorf("no web-search providers available. Add a key in the admin UI under Search Providers")
		}
	}

	if !p.IsAvailable() {
		if p.RequiresAPIKey {
			return nil, "", fmt.Errorf("provider %q has no API key configured", p.Name)
		}
		if !p.IsEnabled {
			return nil, "", fmt.Errorf("provider %q is disabled", p.Name)
		}
	}

	apiKey := ""
	if p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != "" {
		if ctx.Encryptor == nil {
			return nil, "", fmt.Errorf("encryption not available, cannot decrypt provider key")
		}
		dec, err := ctx.Encryptor.Decrypt(*p.APIKeyEncrypted)
		if err != nil {
			return nil, "", fmt.Errorf("decrypting api key for %s: %w", p.Name, err)
		}
		apiKey = strings.TrimSpace(dec)
	}
	return p, apiKey, nil
}
