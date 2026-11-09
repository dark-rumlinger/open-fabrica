package engine

// LLM provider resolution + the permanent-error classifier. providerResolver
// is deliberately independent of the worker so the session loop, scheduled
// tasks, and tests can resolve providers without a full worker.

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/projectmodel"
	"openfabrica/providers/llm"
	"openfabrica/security"
)

// resolvedProvider is one live provider + model binding. A stage the Thinking map
// leaves out follows the engine default (see resolveThinking).
type resolvedProvider projectmodel.Resolved

// providerSource abstracts provider resolution so sessions can be tested with
// a static provider. providerResolver is the production implementation.
type providerSource interface {
	resolve() (resolvedProvider, error)
	fallbackHook() FallbackHook
}

// staticProviderSource always returns one fixed provider; used by tests and
// by callers that already resolved a provider.
type staticProviderSource struct{ rp resolvedProvider }

func (s staticProviderSource) resolve() (resolvedProvider, error) { return s.rp, nil }
func (s staticProviderSource) fallbackHook() FallbackHook         { return nil }

// providerResolver resolves the project's configured provider (falling back
// to the global default) and, on demand, the configured fallback provider.
type providerResolver struct {
	db         *db.DB
	encryptor  *security.Encryptor
	httpClient *http.Client
	projectID  int

	mu                sync.Mutex
	currentProviderID int
}

func newProviderResolver(deps *Deps, projectID int) *providerResolver {
	return &providerResolver{
		db:         deps.DB,
		encryptor:  deps.Encryptor,
		httpClient: deps.LLMHTTPClient,
		projectID:  projectID,
	}
}

// resolve returns the project's provider, falling back to the global default
// when the project has none. The provider id keys the concurrency limiter
// and the fallback lookup.
func (r *providerResolver) resolve() (resolvedProvider, error) {
	res, err := projectmodel.Resolve(r.db.DB, r.encryptor, r.httpClient, r.projectID)
	if err != nil {
		return resolvedProvider{}, err
	}
	r.mu.Lock()
	r.currentProviderID = res.ProviderID
	r.mu.Unlock()
	return resolvedProvider(res), nil
}

// fallback returns an alternative provider on a different LLM provider than
// the current one, or an error when none is configured.
func (r *providerResolver) fallback() (resolvedProvider, error) {
	r.mu.Lock()
	pid := r.currentProviderID
	r.mu.Unlock()
	if pid == 0 {
		return resolvedProvider{}, fmt.Errorf("no current provider to fall back from")
	}
	model, providerRow, err := models.GetFallbackModel(r.db.DB, pid)
	if err != nil {
		return resolvedProvider{}, fmt.Errorf("no fallback provider available: %w", err)
	}
	return r.build(model, providerRow)
}

// fallbackHook adapts the resolver into the retry layer's FallbackHook shape.
func (r *providerResolver) fallbackHook() FallbackHook {
	return func(err error) (llm.Provider, string) {
		// Schema-unsupported means THIS provider can't honour structured
		// output; a different provider might. Everything fatal-for-payload
		// (auth, bad request, quota) would fail identically - don't bother.
		if IsFatalLLMError(err) && !IsSchemaUnsupported(err) {
			return nil, ""
		}
		alt, ferr := r.fallback()
		if ferr != nil {
			return nil, ""
		}
		return alt.Provider, alt.Model
	}
}

// build turns a model+provider row into a live llm.Provider (projectmodel.Build).
func (r *providerResolver) build(model *models.LLMModel, providerRow *models.LLMProvider) (resolvedProvider, error) {
	res, err := projectmodel.Build(r.encryptor, r.httpClient, model, providerRow)
	return resolvedProvider(res), err
}

// isPermanentError returns true for errors that can never succeed on retry
// (missing API keys, unreachable endpoints, bad provider config); the worker's
// circuit breaker pauses immediately instead of burning stage retries. It asks a
// DIFFERENT question from callOnProvider's fatal switch, which also stops on
// CodeTruncated / CodeSchemaDrift: those end one CALL and must never pause the
// WORKER - the next task's payload is a different size and shape and will
// usually go through. IsFatalLLMError draws the line at fatal-for-the-payload,
// the correct input here for exactly that reason.
func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	if IsFatalLLMError(err) {
		return true
	}
	if IsSchemaUnsupported(err) {
		return true
	}
	if isBudgetError(err) {
		return true
	}
	return errors.Is(err, ErrProviderConfig)
}

// ErrProviderConfig marks a provider-resolution failure that only an owner
// change can fix (no model configured, no API key, an undecryptable key): the
// circuit breaker pauses on it instead of spending stage retries. Match it with
// errors.Is; the error text itself is unchanged, since owners read it.
var ErrProviderConfig = projectmodel.ErrConfig

// quotaPauseMessage returns an owner-facing pause message when err is (or
// wraps) a provider quota-exhaustion error, plus true. Returns ("", false)
// for any other error.
func quotaPauseMessage(err error) (string, bool) {
	lerr, ok := llm.AsLLMError(err)
	if !ok || lerr.Code != llm.CodeQuotaExceeded {
		return "", false
	}
	if lerr.RetryAfter > 0 {
		reset := time.Now().Add(lerr.RetryAfter)
		layout := "15:04"
		if lerr.RetryAfter > 12*time.Hour {
			layout = "Jan 2 15:04"
		}
		return fmt.Sprintf(
			msgcat.Mark("I've used up the AI provider's usage quota for now, so I've paused. It resets around %s, and I'll pick back up after that. You can also raise the quota or switch providers to continue sooner."),
			reset.Local().Format(layout),
		), true
	}
	return msgcat.Mark("I've used up the AI provider's usage quota for now, so I've paused. I'll pick back up once it resets. You can also raise the quota or switch providers to continue sooner."), true
}

// supportsStructuredOutput answers both halves of the question: can this
// TRANSPORT constrain a response to a schema, and has this MODEL not already
// been learned unable to. Both engine gates must ask the same question, so it
// lives here rather than being re-asserted at each call site.
func supportsStructuredOutput(rp resolvedProvider) bool {
	sop, ok := rp.Provider.(llm.StructuredOutputProvider)
	if !ok || !sop.SupportsStructuredOutput() {
		return false
	}
	return llm.Caps.Allowed(llm.ModelKey{Provider: rp.Provider.Name(), Model: rp.Model}, llm.CapStructuredOutput)
}
