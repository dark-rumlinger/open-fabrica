package llm

import (
	"openfabrica/internal/providerreg"
)

// ProviderConstructor builds a Provider from an Options value. Backends
// register a constructor under their transport identifier via
// RegisterProvider in init().
type ProviderConstructor = providerreg.Constructor[Provider]

// Options is the construction input for a backend: name, credential, base URL
// root, models URL, HTTP client, and the decoded providers.config blob. Aliased
// from providerreg so callers say the domain name without importing the
// generic registry.
type Options = providerreg.Options

var registry = providerreg.New[Provider]("llm")

// RegisterProvider registers a constructor for a transport. Called from
// the init() of each provider backend package (anthropic, openai, ...).
// Duplicate registrations are recorded on the registry and surfaced via
// RegistrationErrors so main() can report every problem before exiting.
func RegisterProvider(transport string, c ProviderConstructor) {
	_ = registry.Register(transport, c)
}

// RegistrationErrors returns errors collected during init()-time provider
// registration. Drained by main() at startup.
func RegistrationErrors() []error {
	return registry.RegistrationErrors()
}

// NewProvider constructs a Provider for the given transport. Returns an
// error if no backend is registered. See providerreg.Options for the
// per-field defaults, and for the rule that BaseURL is an API root
// rather than a full operation endpoint.
func NewProvider(transport string, opts Options) (Provider, error) {
	return registry.New(transport, opts)
}

// HasTransport reports whether a constructor is registered for the given
// transport identifier. Used by the admin handler to validate provider
// creation requests before hitting the DB.
func HasTransport(transport string) bool {
	return registry.Has(transport)
}

// RegisteredTypes returns the sorted list of transport identifiers that
// have a constructor registered. Symmetric with imagegen/stocksearch.
func RegisteredTypes() []string {
	return registry.Types()
}
