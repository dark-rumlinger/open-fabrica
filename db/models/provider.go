package models

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"openfabrica/internal/providercfg"
	"openfabrica/internal/providerreg"
)

// Executor is satisfied by both *sql.DB and *sql.Tx, allowing model
// functions to work inside or outside a transaction.
type Executor interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// Provider plan values for LLMProvider.Plan.
const (
	PlanStandard = "standard"
	PlanCoding   = "coding"
)

// ValidPlan reports whether plan is a known plan value.
func ValidPlan(plan string) bool {
	return plan == PlanStandard || plan == PlanCoding
}

type LLMProvider struct {
	ID   int    `json:"id" db:"id"`
	Name string `json:"name" db:"name"`
	// Vendor is the stable identity of the upstream service ('anthropic',
	// 'ollama', 'gemini', ...). seed.json upserts rows by matching on this
	// column, so it must be unique and never change once set.
	Vendor string `json:"vendor" db:"vendor"`
	// Transport selects the wire protocol used to talk to the provider
	// ('anthropic', 'openai-chat', 'ollama', 'openai-image', 'automatic1111',
	// 'pexels', ...). The factory keys constructors off this field; multiple
	// vendors may share a transport.
	Transport       string  `json:"transport" db:"transport"`
	APIKeyEncrypted *string `json:"-" db:"api_key_encrypted"`
	BaseURL         *string `json:"base_url" db:"base_url"`
	// ModelsURL is the catalog endpoint used by SyncModels. Some providers
	// (Ollama) expose listing on a different path than chat, so it is
	// stored explicitly rather than derived. Empty means no sync support.
	ModelsURL      string `json:"models_url" db:"models_url"`
	IsEnabled      bool   `json:"is_enabled" db:"is_enabled"`
	RequiresAPIKey bool   `json:"requires_api_key" db:"requires_api_key"`
	// Kind: 'llm', 'image', or 'stock'. Declared in seed.json rather than
	// inferred from package registration, so a single provider list drives
	// every picker via a column filter.
	Kind string `json:"kind" db:"kind"`
	// Plan is how the endpoint is billed: 'standard' (metered API) or 'coding'
	// (a subscription plan endpoint, usually limited to interactive coding
	// tools). Declared in seed.json.
	Plan      string `json:"plan" db:"plan"`
	SignupURL string `json:"signup_url" db:"signup_url"`
	Config    string `json:"config" db:"config"`
	// IsDefault marks this provider as the per-kind default. The resolver
	// for image/stock tools prefers this row when no provider is named.
	// LLM defaults live at model level, so this is a no-op for kind='llm'.
	IsDefault bool      `json:"is_default" db:"is_default"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// IsAvailable reports whether this provider can physically serve requests
// right now: enabled, and either keyless or has a key configured. Single
// source of truth for "can a project actually use this provider".
func (p *LLMProvider) IsAvailable() bool {
	if !p.IsEnabled {
		return false
	}
	if !p.RequiresAPIKey {
		return true
	}
	return p.APIKeyEncrypted != nil && *p.APIKeyEncrypted != ""
}

// providerCols / modelCols are the SELECT column lists, derived once from the
// structs' `db` tags so they always match field order and the Scan targets
// below. The "P"/"M" variants prefix each column with the table alias used in
// joined queries; without prefixes SQLite emits "ambiguous column name: id"
// because both tables share `id`. See scan.go for the single source of truth.
var (
	providerCols  = dbColumns(LLMProvider{})
	providerColsP = dbColumnsAlias(LLMProvider{}, "p")
	modelCols     = dbColumns(LLMModel{})
	modelColsM    = dbColumnsAlias(LLMModel{}, "m")
)

func scanProvider(row interface {
	Scan(dest ...any) error
}, p *LLMProvider) error {
	return row.Scan(dbScanTargets(p)...)
}

func scanModel(row interface {
	Scan(dest ...any) error
}, m *LLMModel) error {
	return row.Scan(dbScanTargets(m)...)
}

func scanModelAndProvider(row interface {
	Scan(dest ...any) error
}, m *LLMModel, p *LLMProvider) error {
	return row.Scan(append(dbScanTargets(m), dbScanTargets(p)...)...)
}

func CreateProvider(db *sql.DB, name, vendor, transport string, apiKeyEncrypted *string, baseURL *string) (*LLMProvider, error) {
	return CreateProviderTx(db, name, vendor, transport, apiKeyEncrypted, baseURL)
}

// CreateProviderTx creates a provider using any Executor (DB or Tx). The new
// row defaults to requires_api_key=true; the seed loader provisions keyless
// providers (Ollama) directly through UpsertProviderTemplate.
func CreateProviderTx(exec Executor, name, vendor, transport string, apiKeyEncrypted *string, baseURL *string) (*LLMProvider, error) {
	result, err := exec.Exec(
		"INSERT INTO providers (name, vendor, transport, api_key_encrypted, base_url) VALUES (?, ?, ?, ?, ?)",
		name, vendor, transport, apiKeyEncrypted, baseURL,
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}
	p := &LLMProvider{}
	if err := scanProvider(exec.QueryRow("SELECT "+providerCols+" FROM providers WHERE id = ?", int(id)), p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetProviderByVendor looks up a provider by its stable vendor identity.
// Returns sql.ErrNoRows when not seeded yet. The seed loader uses this to
// decide whether to insert or refresh template metadata.
func GetProviderByVendor(exec Executor, vendor string) (*LLMProvider, error) {
	p := &LLMProvider{}
	if err := scanProvider(exec.QueryRow("SELECT "+providerCols+" FROM providers WHERE vendor = ?", vendor), p); err != nil {
		return nil, err
	}
	return p, nil
}

// ProviderTemplate holds the non-user-owned provider columns
// UpsertProviderTemplate writes for a row keyed by Vendor. It replaces a
// 10-parameter signature whose seven adjacent strings were easy to transpose.
type ProviderTemplate struct {
	Vendor         string
	Name           string
	Transport      string
	Kind           string
	Plan           string
	BaseURL        string
	ModelsURL      string
	SignupURL      string
	RequiresAPIKey bool
	ConfigJSON     string
}

// UpsertProviderTemplate creates or refreshes the non-user-owned columns of
// a provider row keyed by vendor. User-owned fields (api_key_encrypted,
// is_enabled, custom base_url overrides) are never touched here, so admin
// edits survive a re-seed on every boot.
func UpsertProviderTemplate(exec Executor, t ProviderTemplate) (*LLMProvider, error) {
	vendor := t.Vendor
	name := t.Name
	transport := t.Transport
	kind := t.Kind
	plan := t.Plan
	if plan == "" {
		plan = PlanStandard
	}
	baseURL := t.BaseURL
	modelsURL := t.ModelsURL
	signupURL := t.SignupURL
	requiresAPIKey := t.RequiresAPIKey
	configJSON := t.ConfigJSON

	existing, err := GetProviderByVendor(exec, vendor)
	if err == nil {
		_, err = exec.Exec(`
			UPDATE providers
			SET name = ?, transport = ?, kind = ?, plan = ?, models_url = ?, signup_url = ?, requires_api_key = ?, config = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`,
			name, transport, kind, plan, modelsURL, signupURL, requiresAPIKey, configJSON, existing.ID,
		)
		if err != nil {
			return nil, err
		}
		p := &LLMProvider{}
		if err := scanProvider(exec.QueryRow("SELECT "+providerCols+" FROM providers WHERE id = ?", existing.ID), p); err != nil {
			return nil, err
		}
		return p, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	res, err := exec.Exec(`
		INSERT INTO providers (name, vendor, transport, kind, plan, base_url, models_url, signup_url, requires_api_key, config)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, vendor, transport, kind, plan, nilIfEmpty(baseURL), modelsURL, signupURL, requiresAPIKey, configJSON,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	p := &LLMProvider{}
	if err := scanProvider(exec.QueryRow("SELECT "+providerCols+" FROM providers WHERE id = ?", int(id)), p); err != nil {
		return nil, err
	}
	return p, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EmbeddingModel reads the per-provider embedding model id from the Config JSON,
// or "" when unset. Callers must surface a clear error rather than fall back to a
// hardcoded model.
func (p *LLMProvider) EmbeddingModel() string {
	return providercfg.Parse(p.Config).EmbeddingModel
}

// ProviderOptions builds the transport construction input from this row. The
// single place DB columns are mapped onto providerreg.Options, so every call
// site - engine, chat, seed, the admin diagnostics probe, the content tools -
// constructs a provider identically and a new Options field reaches all of
// them at once. apiKey is passed in already decrypted; hc may be nil.
func (p *LLMProvider) ProviderOptions(apiKey string, hc *http.Client) providerreg.Options {
	baseURL := ""
	if p.BaseURL != nil {
		baseURL = *p.BaseURL
	}
	return providerreg.Options{
		Name:       p.Name,
		APIKey:     apiKey,
		BaseURL:    baseURL,
		ModelsURL:  p.ModelsURL,
		HTTPClient: hc,
		Config:     providercfg.Parse(p.Config),
	}
}

// SetProviderKind updates the kind discriminator on a provider row. Used by
// the seed loader to mark image-generation providers after creation, since
// CreateProvider always defaults the column to 'llm'.
func SetProviderKind(exec Executor, providerID int, kind string) error {
	_, err := exec.Exec(
		"UPDATE providers SET kind = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		kind, providerID,
	)
	return err
}

// ListProvidersByKind returns providers of a specific kind ('llm' or 'image').
// Used by the image-gen tool to enumerate configured image backends without
// also pulling in chat models.
func ListProvidersByKind(db *sql.DB, kind string) ([]LLMProvider, error) {
	rows, err := db.Query(
		"SELECT "+providerCols+" FROM providers WHERE kind = ? ORDER BY id",
		kind,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMProvider
	for rows.Next() {
		var p LLMProvider
		if err := scanProvider(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProviderAsDefaultForKind marks providerID the default for its kind, clearing
// the flag on any other provider of that kind. Transactional so the "exactly one
// default per kind" invariant survives a partial failure.
func SetProviderAsDefaultForKind(db *sql.DB, providerID int) error {
	p, err := GetProviderByID(db, providerID)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"UPDATE providers SET is_default = 0, updated_at = CURRENT_TIMESTAMP WHERE kind = ? AND id != ?",
		p.Kind, providerID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"UPDATE providers SET is_default = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		providerID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetDefaultProviderByKind returns the provider flagged as default for the
// given kind, or sql.ErrNoRows when none is set. Used by the image/stock
// tool resolvers as the preferred pick when the LLM did not name a
// provider explicitly.
func GetDefaultProviderByKind(db *sql.DB, kind string) (*LLMProvider, error) {
	p := &LLMProvider{}
	if err := scanProvider(db.QueryRow(
		"SELECT "+providerCols+" FROM providers WHERE kind = ? AND is_default = 1 LIMIT 1",
		kind,
	), p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetProviderByName looks up a provider by its (unique) name. Returns
// sql.ErrNoRows when not found. Used by tools that resolve a provider
// chosen by the LLM in chat (where only the name is stable across DBs).
func GetProviderByName(db *sql.DB, name string) (*LLMProvider, error) {
	p := &LLMProvider{}
	if err := scanProvider(db.QueryRow("SELECT "+providerCols+" FROM providers WHERE name = ?", name), p); err != nil {
		return nil, err
	}
	return p, nil
}

func GetProviderByID(db *sql.DB, id int) (*LLMProvider, error) {
	p := &LLMProvider{}
	if err := scanProvider(db.QueryRow("SELECT "+providerCols+" FROM providers WHERE id = ?", id), p); err != nil {
		return nil, err
	}
	return p, nil
}

func ListProviders(db *sql.DB) ([]LLMProvider, error) {
	rows, err := db.Query("SELECT " + providerCols + " FROM providers ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []LLMProvider
	for rows.Next() {
		var p LLMProvider
		if err := scanProvider(rows, &p); err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, rows.Err()
}

func CountProviders(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM providers").Scan(&count)
	return count, err
}

// CountProjectsUsingProvider reports how many projects run a model of this provider.
func CountProjectsUsingProvider(db *sql.DB, providerID int) (int, error) {
	var n int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM projects WHERE llm_model_id IN (SELECT id FROM llm_models WHERE provider_id = ?)",
		providerID).Scan(&n)
	return n, err
}

// CountProjectsUsingModel reports how many projects run this model.
func CountProjectsUsingModel(db *sql.DB, modelPK int) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM projects WHERE llm_model_id = ?", modelPK).Scan(&n)
	return n, err
}

func UpdateProvider(db *sql.DB, id int, name string, apiKeyEncrypted *string, baseURL *string, isEnabled bool) error {
	_, err := db.Exec(
		"UPDATE providers SET name = ?, api_key_encrypted = ?, base_url = ?, is_enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		name, apiKeyEncrypted, baseURL, isEnabled, id,
	)
	return err
}

func DeleteProvider(db *sql.DB, id int) error {
	_, err := db.Exec("DELETE FROM providers WHERE id = ?", id)
	return err
}
