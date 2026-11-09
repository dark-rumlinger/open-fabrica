package llm

import (
	"database/sql"
	"encoding/json"
)

// Capabilities is a snapshot of which provider-backed capabilities are
// usable right now: at least one provider of the matching kind is enabled
// and either keyless or has an API key configured. Image additionally
// requires the provider to have an enabled model row, since its generate
// handler resolves a model before it can produce anything. Tools whose
// execution requires a backend provider gate their LLM visibility on this
// snapshot via ToolDef.RequiresCapability, so the model never sees a tool
// that would only error at runtime.
//
// Some capability names are composites over these fields rather than fields
// of their own; Has resolves them.
type Capabilities struct {
	HasLLM        bool
	HasImage      bool
	HasStock      bool
	HasSearch     bool
	HasEmbeddings bool
	HasPush       bool
	// HasPDF is always true: PDF rendering uses the bundled chromedp
	// runtime (no external provider). Tracked here so RequiresCapability
	// gating stays uniform across tools.
	HasPDF bool
}

// Has returns whether the named capability is present. Empty name returns
// true so tools that declare no requirement always pass the filter.
func (c Capabilities) Has(name string) bool {
	switch name {
	case "":
		return true
	case "llm":
		return c.HasLLM
	case "image":
		return c.HasImage
	case "stock":
		return c.HasStock
	case "image_source":
		// Derived, not stored: an image reaches /files by generation or by a
		// stock fetch. Computed here so a hand-built snapshot cannot hold a
		// composite that contradicts its own parts.
		return c.HasImage || c.HasStock
	case "search":
		return c.HasSearch
	case "embeddings":
		return c.HasEmbeddings
	case "push":
		return c.HasPush
	case "pdf":
		return c.HasPDF
	}
	return false
}

// KnownCapabilities is the closed set of PLATFORM capability names tools may
// declare via ToolDef.RequiresCapability: whether any image or search provider
// is configured at all. Not per-model capability, which lives in the separate
// KnownModelCaps set in model_caps.go.
//
// Checked by engine/capability_visibility_test.go. There is no boot-time
// validator for this set, unlike KnownModelCaps.
var KnownCapabilities = map[string]bool{
	"llm":          true,
	"image":        true,
	"stock":        true,
	"image_source": true,
	"search":       true,
	"embeddings":   true,
	"push":         true,
	"pdf":          true,
}

// LoadCapabilities queries the providers table once and returns a snapshot of
// which kinds have at least one available row. Embeddings is derived from any
// available kind='llm' provider whose Config JSON carries an embedding_model field,
// the same rule the OpenAI Embed path uses, so the snapshot and the runtime check
// stay in lockstep.
//
// Image carries one extra clause (the provider must also own an enabled llm_models
// row) because manage_image_gen resolves a model via GetDefaultModelForProvider and
// hard errors when there is none: the seeded, keyless Automatic1111 provider ships
// without a model, so provider-presence alone would surface a tool that can only
// fail when called.
//
// Availability rule mirrors LLMProvider.IsAvailable and the selectableWhere predicate
// in db/models/provider.go: is_enabled=1 AND (requires_api_key=0 OR api_key_encrypted
// IS NOT NULL AND != ”).
func LoadCapabilities(db *sql.DB) (Capabilities, error) {
	const q = `
		SELECT p.kind, p.config,
		       EXISTS(
		           SELECT 1 FROM llm_models m
		           WHERE m.provider_id = p.id AND m.is_enabled = 1
		       ) AS has_enabled_model
		FROM providers p
		WHERE p.is_enabled = 1
		  AND (p.requires_api_key = 0 OR (p.api_key_encrypted IS NOT NULL AND p.api_key_encrypted != ''))
	`
	rows, err := db.Query(q)
	if err != nil {
		return Capabilities{}, err
	}
	defer rows.Close()
	// PDF and push are software-only or per-project-configured.
	// PDF is always on (chromedp is bundled); the other two depend on
	// per-project config rows in the project DB, so they're set true at the
	// platform level and the tool runtime checks the project config when
	// invoked. Gating on global provider rows would over-restrict, since
	// these have no global provider and are configured per project.
	caps := Capabilities{HasPDF: true, HasPush: true}
	for rows.Next() {
		var kind, config string
		var hasEnabledModel int
		if err := rows.Scan(&kind, &config, &hasEnabledModel); err != nil {
			return Capabilities{}, err
		}
		switch kind {
		case "llm":
			caps.HasLLM = true
			if !caps.HasEmbeddings && configHasEmbeddingModel(config) {
				caps.HasEmbeddings = true
			}
		case "image":
			// Provider presence is not enough: without an enabled model row
			// the generate handler can only error, so keep manage_image_gen
			// hidden until a model exists. See LoadCapabilities doc.
			if hasEnabledModel != 0 {
				caps.HasImage = true
			}
		case "stock":
			caps.HasStock = true
		case "search":
			caps.HasSearch = true
		}
	}
	return caps, rows.Err()
}

func configHasEmbeddingModel(raw string) bool {
	if raw == "" {
		return false
	}
	var cfg struct {
		EmbeddingModel string `json:"embedding_model"`
	}
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return false
	}
	return cfg.EmbeddingModel != ""
}
