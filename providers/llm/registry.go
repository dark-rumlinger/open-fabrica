package llm

import (
	"fmt"
	"sync"
)

// Set is a thread-safe registry of configured LLM provider instances.
// Distinct from pkg/providerreg.Registry, which holds provider *constructors*
// keyed by transport type; this holds *instances* keyed by configured name.
type Set struct {
	mu          sync.RWMutex
	providers   map[string]Provider
	defaultName string
}

// NewRegistry creates a new empty provider registry.
func NewRegistry() *Set {
	return &Set{
		providers: make(map[string]Provider),
	}
}

// Register adds a provider to the registry. The first provider registered
// becomes the default unless overridden by SetDefault.
func (r *Set) Register(name string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.providers[name] = p
	if r.defaultName == "" {
		r.defaultName = name
	}
}

// Unregister removes a provider, so a disabled or deleted one stops answering.
// When it was the default, another registered provider takes over (or none).
func (r *Set) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, name)
	if r.defaultName == name {
		r.defaultName = ""
		for other := range r.providers {
			r.defaultName = other
			break
		}
	}
}

// Get returns a provider by name, or an error if not found.
func (r *Set) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("llm: provider %q not found", name)
	}
	return p, nil
}
