package websearch

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

var registry = providerreg.New[Provider]("websearch")

// RegisterProvider registers a constructor for a web-search type.
// Duplicate registrations are recorded and surfaced via RegistrationErrors
// so main() can report every problem before exiting.
func RegisterProvider(providerType string, c ProviderConstructor) {
	_ = registry.Register(providerType, c)
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
func NewProvider(providerType string, opts Options) (Provider, error) {
	return registry.New(providerType, opts)
}

// RegisteredTypes returns the sorted list of provider type identifiers
// that have a constructor registered. Used by the admin handler's
// kindFromTransport lookup so adding a new backend wires through the kind
// discriminator without code changes there.
func RegisteredTypes() []string {
	return registry.Types()
}
