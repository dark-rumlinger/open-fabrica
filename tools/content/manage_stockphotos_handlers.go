package content

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"openfabrica/internal/netguard"
	"openfabrica/tools"

	"openfabrica/db/models"
	"openfabrica/providers/stocksearch"
)

// manage_stockphotos, search and fetch topical free-licensed photos via
// the configured stock-search providers (Pexels, Unsplash, Pixabay).
// Provider rows live in providers with kind='stock'; supported types
// are registered in the stocksearch package. API keys come from the
// `api_key_env` declared in seed.json (env var) or the encrypted column
// on the provider row.

func stockPhotosSearch(ctx *tools.ToolContext, query, providerName string, count int) (*tools.Result, error) {
	if strings.TrimSpace(query) == "" {
		return tools.Fail("query is required"), nil
	}

	provider, gen, err := buildStockProvider(ctx, providerName)
	if err != nil {
		return tools.FailErr(err), nil
	}

	resp, err := gen.Search(ctx.Context(), stocksearch.SearchRequest{
		Query: query,
		Count: clampCount(count),
	})
	if err != nil {
		return tools.FailErr(err), nil
	}

	return tools.Ok(map[string]any{
		"provider": provider.Name,
		"query":    query,
		"count":    len(resp.Results),
		"results":  resp.Results,
	}), nil
}

func stockPhotosFetch(ctx *tools.ToolContext, query string, count int, size, providerName string) (*tools.Result, error) {
	if strings.TrimSpace(query) == "" {
		return tools.Fail("query is required"), nil
	}

	provider, gen, err := buildStockProvider(ctx, providerName)
	if err != nil {
		return tools.FailErr(err), nil
	}

	if size == "" {
		size = "large"
	}

	prefix := slugifyPrefix(query)

	resp, err := gen.Search(ctx.Context(), stocksearch.SearchRequest{
		Query:     query,
		Count:     clampCount(count),
		PhotoSize: size,
	})
	if err != nil {
		return tools.FailErr(err), nil
	}
	if len(resp.Results) == 0 {
		return tools.Fail("no photos found for query %q", query), nil
	}

	type savedPhoto struct {
		Filename     string `json:"filename"`
		URL          string `json:"url"`
		Photographer string `json:"photographer"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
	}
	var saved []savedPhoto
	var failures []string

	for i, r := range resp.Results {
		if r.URL == "" {
			failures = append(failures, fmt.Sprintf("result %d: empty URL (size %q not available)", i+1, size))
			continue
		}
		filename := fmt.Sprintf("%s-%d.jpg", prefix, i+1)
		alt := r.Alt
		if alt == "" {
			alt = query
		}
		// Always include attribution when the backend supplies one
		// (Unsplash requires it; Pexels/Pixabay return "" so the line
		// just appends nothing).
		if r.Attribution != "" {
			alt = fmt.Sprintf("%s, %s", alt, r.Attribution)
		} else if r.Photographer != "" {
			alt = fmt.Sprintf("%s, Photo by %s on %s", alt, r.Photographer, provider.Name)
		}

		if err := fetchImageToArtifact(ctx, r.URL, filename, alt); err != nil {
			failures = append(failures, filename+": "+err.Error())
			continue
		}

		saved = append(saved, savedPhoto{
			Filename:     filename,
			URL:          "/files/" + filename,
			Photographer: r.Photographer,
			Width:        r.Width,
			Height:       r.Height,
		})
	}

	out := map[string]any{
		"provider":  provider.Name,
		"query":     query,
		"saved":     saved,
		"saved_n":   len(saved),
		"requested": len(resp.Results),
	}
	if len(failures) > 0 {
		out["failures"] = failures
	}
	if len(saved) == 0 {
		return &tools.Result{Success: false, Error: fmt.Sprintf("all %d downloads failed", len(failures)), Data: out}, nil
	}
	return tools.Ok(out), nil
}

// buildStockProvider resolves a stock-search provider row from the global
// DB and constructs a ready-to-use stocksearch.Provider. Mirrors the
// shape of resolveImageProvider in image_gen.go: provider lookup by name
// or "first available", key resolution from env-var -> encrypted DB
// column, kind-discriminator enforcement.
func buildStockProvider(ctx *tools.ToolContext, name string) (*models.LLMProvider, stocksearch.Provider, error) {
	provider, apiKey, err := resolveStockProviderRow(ctx, name)
	if err != nil {
		return nil, nil, err
	}

	gen, err := stocksearch.NewProvider(provider.Transport, provider.ProviderOptions(apiKey, nil))
	if err != nil {
		return nil, nil, fmt.Errorf("constructing stock provider: %s. Registered types: %v", err.Error(), stocksearch.RegisteredTypes())
	}
	return provider, gen, nil
}

// resolveStockProviderRow returns the LLMProvider row to use plus the
// resolved API key. Key resolution order:
//  1. Env var declared on the row's seed entry (api_key_env).
//  2. Encrypted column on the provider row.
func resolveStockProviderRow(ctx *tools.ToolContext, name string) (*models.LLMProvider, string, error) {
	db := ctx.GlobalDBOrProject()

	var p *models.LLMProvider
	if name != "" {
		var err error
		p, err = models.GetProviderByName(db, name)
		if err == sql.ErrNoRows {
			return nil, "", fmt.Errorf("no provider named %q, call manage_stockphotos with a known provider or check the admin UI", name)
		}
		if err != nil {
			return nil, "", fmt.Errorf("looking up provider: %w", err)
		}
		if p.Kind != "stock" {
			return nil, "", fmt.Errorf("provider %q is kind=%q, not 'stock'", name, p.Kind)
		}
	} else {
		// Prefer the admin-flagged default for this kind. Only fall back
		// to first-available-by-id when no default is set or the default
		// row is not currently usable (key removed, disabled, ...).
		if def, err := models.GetDefaultProviderByKind(db, "stock"); err == nil && def.IsAvailable() {
			p = def
		} else {
			providers, err := models.ListProvidersByKind(db, "stock")
			if err != nil {
				return nil, "", fmt.Errorf("listing stock providers: %w", err)
			}
			for i := range providers {
				if providers[i].IsAvailable() {
					p = &providers[i]
					break
				}
			}
		}
		if p == nil {
			return nil, "", fmt.Errorf("no stock-search providers available. Add a key in the admin UI under Stock Photo Providers")
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

// clampCount clamps the requested count to [1, 20] with default 6.
// Shared across the content tools (stock-photo search/fetch and web search).
func clampCount(count int) int {
	if count < 1 {
		count = 6
	}
	if count > 20 {
		count = 20
	}
	return count
}

// slugifyPrefix turns a free-form query into a short filename-safe prefix. A query
// whose slug does not fit gets a short hash of the whole query as its tail, so two long
// prompts that share their first words ("professional photograph of a ...") never land on
// the same filename and silently overwrite each other's images.
func slugifyPrefix(query string) string {
	full := slugifyAll(query)
	if len(full) <= 24 {
		if full == "" {
			return "photo"
		}
		return full
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query))))
	head := strings.Trim(full[:19], "-")
	return head + "-" + hex.EncodeToString(sum[:])[:4]
}

// slugifyAll lowercases a query and keeps letters, digits and single dashes.
func slugifyAll(query string) string {
	var b strings.Builder
	last := byte(0)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			last = c
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
			last = c + 32
		case (c == ' ' || c == '-' || c == '_') && last != '-':
			b.WriteByte('-')
			last = '-'
		}
	}
	return strings.Trim(b.String(), "-")
}

// fetchImageToArtifact downloads an image URL and records it in of_files
// under kind=file with scope=global. SSRF is enforced by a pre-flight
// netguard.ValidateExternalURL plus netguard.SSRFSafeClient, which re-checks the resolved
// IP at connect time and on every redirect hop (DNS-rebinding safe). Size is
// capped at 10 MB. Delegates the disk + DB write to SaveImageBytesAsArtifact so
// manage_image_gen and manage_stockphotos store images identically.
func fetchImageToArtifact(ctx *tools.ToolContext, rawURL, filename, altText string) error {
	if err := netguard.ValidateExternalURL(rawURL); err != nil {
		return fmt.Errorf("blocked URL: %w", err)
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	client := netguard.SSRFSafeClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return fmt.Errorf("not an image (content-type %q)", contentType)
	}
	const maxSize = 10 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if len(data) > maxSize {
		return fmt.Errorf("image exceeds %d bytes", maxSize)
	}

	if _, err := SaveImageBytesAsArtifact(ctx, data, contentType, filename, altText); err != nil {
		return err
	}
	return nil
}
