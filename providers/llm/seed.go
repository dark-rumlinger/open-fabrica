package llm

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"openfabrica/db/models"
	"openfabrica/internal/providercfg"
	"openfabrica/security"
)

// SeedConfig is the on-disk shape of seed.json: a single flat list of
// provider templates. Each entry declares its own kind ('llm', 'image',
// 'stock'), so the loader has no implicit positional rule. LLM and image
// catalogs are discovered at runtime via the provider's ListModels
// implementation; only stock-search providers carry a default_model in
// seed.json since their "model" is a single synthetic search endpoint
// with no upstream catalog to enumerate.
type SeedConfig struct {
	Providers []SeedProvider `json:"providers"`
}

type SeedProvider struct {
	Vendor    string `json:"vendor"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	// Plan is how the endpoint is billed: "standard" (metered, the default) or
	// "coding" (a subscription plan endpoint). Shown as a badge in the admin UI.
	Plan string `json:"plan,omitempty"`
	// BaseURL is an API ROOT ("https://api.example.com/v1"), never a full
	// operation endpoint. The transport appends the path.
	BaseURL string `json:"base_url,omitempty"`
	// ModelsURL is authoritative when set and derived from BaseURL when not,
	// which is how a provider that lists on an unrelated path stays a config
	// row rather than a Go package.
	ModelsURL    string             `json:"models_url,omitempty"`
	SignupURL    string             `json:"signup_url,omitempty"`
	Auth         SeedAuth           `json:"auth"`
	Config       providercfg.Config `json:"config,omitempty"`
	DefaultModel *SeedModel         `json:"default_model,omitempty"`
}

// SeedAuth is both the key-sourcing policy (Env, Required) and the credential
// PLACEMENT descriptor. The placement half is folded into the stored config on
// upsert, so a vendor with an unusual auth header is a seed row rather than a
// transport: Env/Required drive columns, the rest drives providercfg.Auth.
type SeedAuth struct {
	Env      string `json:"env,omitempty"`
	Required *bool  `json:"required,omitempty"`
	Scheme   string `json:"scheme,omitempty"`
	Header   string `json:"header,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Param    string `json:"param,omitempty"`
}

// descriptor is the placement half of SeedAuth, as the transports consume it.
func (a SeedAuth) descriptor() providercfg.Auth {
	return providercfg.Auth{Scheme: a.Scheme, Header: a.Header, Prefix: a.Prefix, Param: a.Param}
}

// SeedModel is the bootstrap model row for providers whose catalog is
// not discoverable (stock photo search). Listable providers (LLM, image
// generation) leave this empty and rely on a Sync after key entry.
type SeedModel struct {
	ID                string `json:"id"`
	DisplayName       string `json:"display_name"`
	ContextTokens     int    `json:"context_tokens,omitempty"`
	MaxOutputTokens   int    `json:"max_output_tokens,omitempty"`
	SupportsStreaming bool   `json:"supports_streaming,omitempty"`
	SupportsTools     bool   `json:"supports_tools,omitempty"`
	SupportsVision    bool   `json:"supports_vision,omitempty"`
}

// LoadSeed reads seed.json (or the embedded copy when no file exists) and upserts
// provider templates on every boot.
// Existing rows have their template fields (name, transport, kind,
// models_url, signup_url, requires_api_key, config) refreshed in place
// without touching user-owned columns (api_key_encrypted, is_enabled,
// base_url overrides) so admin edits survive a restart.
//
// LLM providers configured via env-var keys are registered into the live
// Providers instance registry. Image/stock providers are instantiated on
// demand by their respective tools, not here.
func LoadSeed(path string, embedded []byte, db *sql.DB, enc *security.Encryptor, registry *Set, hc *http.Client) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("seed: read file: %w", err)
		}
		if len(embedded) == 0 {
			slog.Debug("seed: no seed file found", "path", path)
			return nil
		}
		slog.Info("seed: no seed file on disk, using the embedded seed", "path", path)
		data = embedded
	}

	var cfg SeedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("seed: parse config: %w", err)
	}

	slog.Info("seed: applying provider templates", "count", len(cfg.Providers))

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("seed: begin transaction: %w", err)
	}
	defer tx.Rollback()

	for _, sp := range cfg.Providers {
		if err := upsertSeedProvider(tx, enc, sp); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("seed: commit: %w", err)
	}

	// Register live LLM provider instances using the now-persisted rows so
	// the registry reflects DB truth (including any user-set API keys), not
	// just env-var keys. Image and stock providers are constructed on
	// demand by their tools and are not registered here.
	if registry != nil {
		if err := registerLLMProviders(db, enc, registry, hc); err != nil {
			slog.Warn("seed: register live providers failed", "error", err)
		}
	}

	slog.Info("seed: template apply complete")
	return nil
}

func upsertSeedProvider(tx *sql.Tx, enc *security.Encryptor, sp SeedProvider) error {
	if sp.Vendor == "" || sp.Kind == "" || sp.Transport == "" || sp.Name == "" {
		return fmt.Errorf("seed: provider missing required fields: %+v", sp)
	}
	if sp.Plan != "" && !models.ValidPlan(sp.Plan) {
		return fmt.Errorf("seed: %s: unknown plan %q", sp.Vendor, sp.Plan)
	}

	requiresKey := true
	if sp.Auth.Required != nil {
		requiresKey = *sp.Auth.Required
	}

	// The auth block is the single home for credential placement: fold it into
	// the config the transports read, so seed.json never states it twice.
	existed := true
	if _, err := models.GetProviderByVendor(tx, sp.Vendor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			existed = false
		} else {
			return fmt.Errorf("seed: lookup %s: %w", sp.Vendor, err)
		}
	}

	cfg := sp.Config
	cfg.Auth = sp.Auth.descriptor()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("seed: %s: %w", sp.Vendor, err)
	}
	configJSON, err := cfg.Marshal()
	if err != nil {
		return fmt.Errorf("seed: marshal config for %s: %w", sp.Vendor, err)
	}

	provider, err := models.UpsertProviderTemplate(tx, models.ProviderTemplate{
		Vendor:         sp.Vendor,
		Name:           sp.Name,
		Transport:      sp.Transport,
		Kind:           sp.Kind,
		Plan:           sp.Plan,
		BaseURL:        sp.BaseURL,
		ModelsURL:      sp.ModelsURL,
		SignupURL:      sp.SignupURL,
		RequiresAPIKey: requiresKey,
		ConfigJSON:     configJSON,
	})
	if err != nil {
		return fmt.Errorf("seed: upsert %s: %w", sp.Vendor, err)
	}

	// Apply env-var API key only on first insert. On subsequent boots the
	// admin may have rotated/cleared the key from the UI; we must not
	// silently replant a stale env value over user intent.
	if !existed && sp.Auth.Env != "" {
		if envVal := os.Getenv(sp.Auth.Env); envVal != "" {
			encrypted, err := enc.Encrypt(envVal)
			if err != nil {
				return fmt.Errorf("seed: encrypt key for %s: %w", sp.Vendor, err)
			}
			if _, err := tx.Exec(
				"UPDATE providers SET api_key_encrypted = ? WHERE id = ?",
				encrypted, provider.ID,
			); err != nil {
				return fmt.Errorf("seed: store key for %s: %w", sp.Vendor, err)
			}
		}
	}

	// Seed a default model only for providers that do not support catalog
	// discovery (stock search). LLM and image providers leave this empty
	// and rely on the user clicking "Sync Models" after entering a key.
	if !existed && sp.DefaultModel != nil {
		dm := sp.DefaultModel
		if _, err := models.CreateModelTx(tx, provider.ID, models.NewModel{
			ModelID:           dm.ID,
			DisplayName:       dm.DisplayName,
			ContextTokens:     dm.ContextTokens,
			MaxOutputTokens:   dm.MaxOutputTokens,
			SupportsStreaming: dm.SupportsStreaming,
			SupportsTools:     dm.SupportsTools,
			SupportsVision:    dm.SupportsVision,
			IsDefault:         true,
		}); err != nil {
			return fmt.Errorf("seed: create default model for %s: %w", sp.Vendor, err)
		}
	}

	slog.Debug("seed: upserted provider", "vendor", sp.Vendor, "kind", sp.Kind, "transport", sp.Transport, "id", provider.ID, "new", !existed)
	return nil
}

// registerLLMProviders builds live Provider instances for every enabled
// kind='llm' row that has either a key or is keyless, and registers them
// in the runtime registry under the provider's DB Name. Idempotent.
func registerLLMProviders(db *sql.DB, enc *security.Encryptor, registry *Set, hc *http.Client) error {
	rows, err := models.ListProviders(db)
	if err != nil {
		return err
	}
	for _, p := range rows {
		if p.Kind != "llm" || !p.IsAvailable() {
			continue
		}
		var apiKey string
		if p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != "" {
			plain, err := enc.Decrypt(*p.APIKeyEncrypted)
			if err != nil {
				slog.Warn("seed: decrypt key failed", "vendor", p.Vendor, "error", err)
				continue
			}
			apiKey = plain
		}
		live, err := NewProvider(strings.ToLower(p.Transport), p.ProviderOptions(apiKey, hc))
		if err != nil {
			slog.Warn("seed: unsupported transport", "name", p.Name, "transport", p.Transport, "error", err)
			continue
		}
		registry.Register(p.Name, live)
	}
	// Declare what the synced rows say about every model, so the first request
	// of a session already knows rather than learning a 400 at a time.
	if err := SeedCapsFromDB(db); err != nil {
		slog.Warn("seed: model capability seeding failed", "error", err)
	}
	return nil
}
