// Package projectmodel resolves the LLM a project runs on: the project's own model,
// else the instance default, built into a live provider. It is the one resolver for
// every caller (the engine, the public AI endpoints, and the trigger, job and server
// function completions), so they all answer with the model the owner picked for the
// project and share the instance's LLM HTTP client.
package projectmodel

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"openfabrica/db/models"
	"openfabrica/providers/llm"
	"openfabrica/security"
)

// ErrConfig marks a resolution failure no retry can fix: no model configured, a
// missing or undecryptable API key. Match it with errors.Is; the error text is left
// as written, since owners read it.
var ErrConfig = errors.New("provider configuration")

type configErr struct{ err error }

func (e configErr) Error() string        { return e.err.Error() }
func (e configErr) Unwrap() error        { return e.err }
func (e configErr) Is(target error) bool { return target == ErrConfig }

// ConfigError tags err as ErrConfig without changing its text.
func ConfigError(err error) error { return configErr{err} }

// Resolved is one live provider + model binding.
type Resolved struct {
	Provider   llm.Provider
	Model      string
	ProviderID int
	// Thinking is the model's per-stage reasoning switch (llm_models.config).
	Thinking map[string]bool
}

// Resolve returns the project's configured model, falling back to the instance
// default when the project has none. hc is the shared LLM HTTP client (nil uses the
// transport's default).
func Resolve(globalDB *sql.DB, enc *security.Encryptor, hc *http.Client, projectID int) (Resolved, error) {
	model, providerRow, err := models.GetModelForProject(globalDB, projectID)
	if err != nil {
		model, providerRow, err = models.GetDefaultModel(globalDB)
		if err != nil {
			return Resolved{}, ConfigError(fmt.Errorf("no model configured and no default available: %w", err))
		}
	}
	return Build(enc, hc, model, providerRow)
}

// Build turns a model + provider row into a live provider. It re-declares the model's
// capability flags on every build, so an admin edit takes effect on the next call.
func Build(enc *security.Encryptor, hc *http.Client, model *models.LLMModel, providerRow *models.LLMProvider) (Resolved, error) {
	var apiKey string
	if providerRow.APIKeyEncrypted != nil && *providerRow.APIKeyEncrypted != "" {
		if enc == nil {
			return Resolved{}, ConfigError(fmt.Errorf("no encryptor to open the API key for %q", providerRow.Name))
		}
		k, err := enc.Decrypt(*providerRow.APIKeyEncrypted)
		if err != nil {
			return Resolved{}, ConfigError(fmt.Errorf("failed to decrypt API key for %q: %w", providerRow.Name, err))
		}
		apiKey = k
	} else if providerRow.RequiresAPIKey {
		return Resolved{}, ConfigError(fmt.Errorf("provider %q has no API key", providerRow.Name))
	}
	p, err := llm.NewProvider(providerRow.Transport, providerRow.ProviderOptions(apiKey, hc))
	if err != nil {
		return Resolved{}, fmt.Errorf("provider %q: %w", providerRow.Name, err)
	}
	llm.Caps.DeclareFromModelRow(providerRow.Name, model)
	return Resolved{Provider: p, Model: model.ModelID, ProviderID: providerRow.ID, Thinking: model.Thinking()}, nil
}
