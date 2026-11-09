package content

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"openfabrica/tools"

	"openfabrica/db/models"
	"openfabrica/providers/imagegen"
)

// manage_image_gen, generate AI images from a prompt and save them as
// kind=file rows. Provider rows live in providers with kind='image';
// supported types are registered in the imagegen package (openai-image,
// automatic1111). API-key resolution mirrors the LLM path: env var fallback
// is not currently honoured here, operators configure providers via seed.json
// or the admin UI, both of which encrypt the key into the providers table.

// resolveImageProvider returns the LLMProvider row to use, the provider's
// default model row (or nil when no models are configured), and the
// decrypted API key (empty if keyless). When name is empty, picks the
// first available image provider. The default-model lookup is a soft
// miss: callers can still satisfy the request with an explicit model arg.
func resolveImageProvider(ctx *tools.ToolContext, name string) (*models.LLMProvider, *models.LLMModel, string, error) {
	db := ctx.GlobalDBOrProject()
	var p *models.LLMProvider
	if name != "" {
		var err error
		p, err = models.GetProviderByName(db, name)
		if err == sql.ErrNoRows {
			return nil, nil, "", fmt.Errorf("no provider named %q, call list_providers to see configured image backends", name)
		}
		if err != nil {
			return nil, nil, "", fmt.Errorf("looking up provider: %w", err)
		}
		if p.Kind != "image" {
			return nil, nil, "", fmt.Errorf("provider %q is kind=%q, not 'image'", name, p.Kind)
		}
	} else {
		// Prefer the admin-flagged default for this kind. Only fall back
		// to first-available-by-id when no default is set or the default
		// row is not currently usable (key removed, disabled, ...).
		if def, err := models.GetDefaultProviderByKind(db, "image"); err == nil && def.IsAvailable() {
			p = def
		} else {
			providers, err := models.ListProvidersByKind(db, "image")
			if err != nil {
				return nil, nil, "", fmt.Errorf("listing image providers: %w", err)
			}
			for i := range providers {
				if providers[i].IsAvailable() {
					p = &providers[i]
					break
				}
			}
		}
		if p == nil {
			return nil, nil, "", fmt.Errorf("no image-generation providers configured. Add one via the admin UI under Image Generation Providers")
		}
	}

	if !p.IsAvailable() {
		if p.RequiresAPIKey {
			return nil, nil, "", fmt.Errorf("provider %q has no API key configured", p.Name)
		}
		if !p.IsEnabled {
			return nil, nil, "", fmt.Errorf("provider %q is disabled", p.Name)
		}
	}

	apiKey := ""
	if p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != "" {
		if ctx.Encryptor == nil {
			return nil, nil, "", fmt.Errorf("encryption not available, cannot decrypt provider key")
		}
		dec, err := ctx.Encryptor.Decrypt(*p.APIKeyEncrypted)
		if err != nil {
			return nil, nil, "", fmt.Errorf("decrypting api key for %s: %w", p.Name, err)
		}
		apiKey = strings.TrimSpace(dec)
	}

	// Soft-resolve the provider's default model. sql.ErrNoRows means no
	// enabled models are seeded for this provider, callers handle that
	// only if no explicit model arg was supplied.
	defaultModel, err := models.GetDefaultModelForProvider(db, p.ID)
	if err == sql.ErrNoRows {
		defaultModel = nil
	} else if err != nil {
		return nil, nil, "", fmt.Errorf("looking up default model for %s: %w", p.Name, err)
	}
	return p, defaultModel, apiKey, nil
}

func imageGenerate(ctx *tools.ToolContext, prompt, size, alt, filename, providerName, model string, count int) (*tools.Result, error) {
	if strings.TrimSpace(prompt) == "" {
		return tools.Fail("prompt is required"), nil
	}

	provider, defaultModel, apiKey, err := resolveImageProvider(ctx, providerName)
	if err != nil {
		return tools.FailErr(err), nil
	}

	// Model resolution order: explicit arg, then provider's is_default
	// model in llm_models, else error. An empty model id at the API
	// boundary is a loud failure the operator can fix in seed.json or
	// admin instead of a silent guess.
	modelID := strings.TrimSpace(model)
	if modelID == "" {
		if defaultModel == nil {
			return tools.Fail(
				"no model specified and provider %q has no default model configured. "+
					"Set is_default=1 on a row in llm_models for this provider, or pass an explicit model arg.",
				provider.Name), nil
		}
		modelID = defaultModel.ModelID
	}

	gen, err := imagegen.NewProvider(provider.Transport, provider.ProviderOptions(apiKey, nil))
	if err != nil {
		return tools.Fail("constructing provider: %s. Registered types: %v", err.Error(), imagegen.RegisteredTypes()), nil
	}

	if count <= 0 {
		count = 1
	}
	req := imagegen.GenerateRequest{
		Prompt: prompt,
		Model:  modelID,
		Size:   size,
		N:      count,
	}

	resp, err := gen.Generate(ctx.Context(), req)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if len(resp.Images) == 0 {
		return tools.Fail("provider returned no images"), nil
	}

	// The action's `filename` arg overrides the auto-generated prefix. When a
	// caller supplies it, the explicit name must win over the auto-slug derived
	// from the prompt.
	// A passed extension is dropped: the saved name always ends -<n><real ext>, so
	// "hero.jpg" must not become "hero.jpg-1.png".
	prefix := strings.TrimSpace(filename)
	prefix = strings.TrimSuffix(prefix, filepath.Ext(prefix))
	if prefix == "" {
		prefix = slugifyPrefix(prompt)
		if prefix == "" {
			prefix = "image"
		}
	}
	altText := alt

	saved := []map[string]any{}
	failures := []map[string]any{}
	for i, img := range resp.Images {
		ext := ExtForContentType(img.ContentType)
		filename := fmt.Sprintf("%s-%d%s", prefix, i+1, ext)
		// Cap each saved image at 20MB, generated images can be large at
		// 1024x1792 high quality; the stockphotos 10MB limit is too tight.
		if len(img.Bytes) > 20<<20 {
			failures = append(failures, map[string]any{
				"filename": filename,
				"error":    fmt.Sprintf("image exceeds 20MB cap (got %d bytes)", len(img.Bytes)),
			})
			continue
		}
		if _, err := SaveImageBytesAsArtifact(ctx, img.Bytes, img.ContentType, filename, altText); err != nil {
			failures = append(failures, map[string]any{
				"filename": filename,
				"error":    err.Error(),
			})
			continue
		}
		entry := map[string]any{
			"path":         filename,
			"url":          "/files/" + filename,
			"content_type": img.ContentType,
			"size":         len(img.Bytes),
		}
		if img.Seed != 0 {
			entry["seed"] = img.Seed
		}
		if img.Revised != "" {
			entry["revised_prompt"] = img.Revised
		}
		saved = append(saved, entry)
	}

	out := map[string]any{
		"provider": provider.Name,
		"images":   saved,
	}
	if len(failures) > 0 {
		out["failures"] = failures
	}
	if len(saved) == 0 {
		return &tools.Result{Success: false, Error: fmt.Sprintf("all %d images failed to save", len(failures)), Data: out}, nil
	}
	return tools.Ok(out), nil
}

func imageListProviders(ctx *tools.ToolContext) (*tools.Result, error) {
	db := ctx.GlobalDBOrProject()
	providers, err := models.ListProvidersByKind(db, "image")
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
		"registered_types": imagegen.RegisteredTypes(),
	}), nil
}

func imageListModels(ctx *tools.ToolContext, providerName string) (*tools.Result, error) {
	provider, _, _, err := resolveImageProvider(ctx, providerName)
	if err != nil {
		return tools.FailErr(err), nil
	}
	db := ctx.GlobalDBOrProject()
	mList, err := models.ListModelsByProvider(db, provider.ID)
	if err != nil {
		return tools.FailErr(err), nil
	}
	out := make([]map[string]any, 0, len(mList))
	for _, m := range mList {
		out = append(out, map[string]any{
			"model_id":     m.ModelID,
			"display_name": m.DisplayName,
			"is_enabled":   m.IsEnabled,
		})
	}
	return tools.Ok(map[string]any{
		"provider": provider.Name,
		"models":   out,
	}), nil
}

func ExtForContentType(ct string) string {
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".png"
}
