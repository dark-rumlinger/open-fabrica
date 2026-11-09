package tools

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry holds all registered tool definitions keyed by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*ToolDef
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*ToolDef)}
}

// defaultRegistry is the package-level registry populated by init() in each tool file.
var defaultRegistry = NewRegistry()

// registrationErrors accumulates problems encountered during package init() so
// that main can fail with the full list instead of a single panic on the first
// offender. RegistrationErrors() drains the list for inspection at startup.
var (
	registrationMu     sync.Mutex
	registrationErrors []error
)

// add inserts a tool definition. It is the internal sink that Define() funnels into,
// unexported so the public registration API has exactly one entry point. Returns the
// problem rather than panicking so main() can report every one at once; see
// RegistrationErrors.
func (r *Registry) add(def ToolDef) error {
	if def.Name == "" {
		return fmt.Errorf("tools.Define: empty Name")
	}
	if def.OneLiner == "" {
		return fmt.Errorf("tools.Define: %s has empty OneLiner", def.Name)
	}
	if len(def.Actions) == 0 {
		return fmt.Errorf("tools.Define: %s has no Actions", def.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[def.Name]; dup {
		return fmt.Errorf("tools.Define: duplicate name %s", def.Name)
	}
	cp := def
	// Derive ReadOnlyActions from ActionMeta when the caller did not declare
	// it explicitly. Keeps the per-action ReadOnly flag the single source of
	// truth while preserving the build loop's existing map-based lookup.
	if cp.ReadOnlyActions == nil && len(cp.ActionMeta) > 0 {
		ro := map[string]bool{}
		for name, meta := range cp.ActionMeta {
			if meta.ReadOnly {
				ro[name] = true
			}
		}
		if len(ro) > 0 {
			cp.ReadOnlyActions = ro
		}
	}
	r.tools[def.Name] = &cp
	return nil
}

// Clone returns an independent registry holding the same definitions. Tests
// clone the default registry and Define fixtures into the copy, so the shared
// catalogue keeps its production invariants (and Validate stays assertable).
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := NewRegistry()
	for name, def := range r.tools {
		out.tools[name] = def
	}
	return out
}

func recordRegistrationError(err error) {
	registrationMu.Lock()
	registrationErrors = append(registrationErrors, err)
	registrationMu.Unlock()
}

// RegistrationErrors returns the list of errors collected during tool init().
func RegistrationErrors() []error {
	registrationMu.Lock()
	defer registrationMu.Unlock()
	if len(registrationErrors) == 0 {
		return nil
	}
	out := make([]error, len(registrationErrors))
	copy(out, registrationErrors)
	return out
}

// DefaultRegistry returns the package-level registry populated by init().
func DefaultRegistry() *Registry { return defaultRegistry }

// Get retrieves a tool definition by name.
func (r *Registry) Get(name string) (*ToolDef, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("tool not found: %s", name)
	}
	return t, nil
}

func (r *Registry) sortedNames() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// List returns all registered tool definitions in alphabetical order.
func (r *Registry) List() []*ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ToolDef, 0, len(r.tools))
	for _, name := range r.sortedNames() {
		out = append(out, r.tools[name])
	}
	return out
}

// Count returns the number of registered tools.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Has returns true if a tool with the given name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

// OneLinerWarnings returns one warning per registered action whose name appears in
// fallbackReadOnlyActions but whose tool has not explicitly classified it read-only. Those
// actions silently inherit the fallback's read-only treatment, which is wrong for a "search"
// action that mutates or a "get" with side effects. Callers log the returned strings so the
// misclassification surfaces before the first run, not later as a stuck read-only streak.
//
// Returns nil when every fallback-named action is explicitly declared.
func (r *Registry) OneLinerWarnings(fallbackReadOnlyActions map[string]bool) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, name := range r.sortedNames() {
		d := r.tools[name]
		for actionName := range d.Actions {
			if !fallbackReadOnlyActions[actionName] {
				continue
			}
			if d.ReadOnlyActions[actionName] {
				continue
			}
			out = append(out, fmt.Sprintf(
				"tool %s: action %q matches a fallback read-only name but is not declared ReadOnly. "+
					"Set ActionMeta[%q].ReadOnly = true if it really is read-only, or rename the action.",
				d.Name, actionName, actionName,
			))
		}
	}
	return out
}

// Validate runs every structural invariant the framework enforces against
// the current registry. Called from main() at startup (fatal on error).
// Makes drift a boot-time failure rather than a silent runtime footgun.
func (r *Registry) Validate() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var errs []string
	for _, name := range r.sortedNames() {
		d := r.tools[name]
		ctx := "tool " + d.Name
		if d.Category == "" {
			errs = append(errs, ctx+": Category is empty")
		}
		if n := len(d.OneLiner); n > 130 {
			errs = append(errs, fmt.Sprintf("%s: OneLiner is %d chars (max 130)", ctx, n))
		}
		if strings.TrimSpace(d.WhenToUse) == "" {
			errs = append(errs, ctx+": WhenToUse is empty (routing guidance is mandatory)")
		}
		for _, p := range d.Params {
			if strings.TrimSpace(p.Description) == "" {
				errs = append(errs, fmt.Sprintf("%s: param %q has empty Description", ctx, p.Name))
			}
		}
		for actionName, handler := range d.Actions {
			if handler == nil {
				errs = append(errs, fmt.Sprintf("%s: action %s has nil Handler", ctx, actionName))
			}
			meta, hasMeta := d.ActionMeta[actionName]
			if !hasMeta {
				errs = append(errs, fmt.Sprintf("%s: action %s has no ActionMeta entry", ctx, actionName))
				continue
			}
			if strings.TrimSpace(meta.Summary) == "" {
				errs = append(errs, fmt.Sprintf("%s: action %s has empty Summary", ctx, actionName))
			}
			if len(meta.Examples) == 0 {
				errs = append(errs, fmt.Sprintf("%s: action %s has no Examples", ctx, actionName))
			}
			// Each Example.Args must parse against the action's param schema.
			// Catches drift like an example referencing a removed/renamed param,
			// a wrong type, or an enum value the schema no longer accepts.
			for i, ex := range meta.Examples {
				if res := validateArgs(d, ex.Args); res != nil {
					label := fmt.Sprintf("example[%d]", i)
					if ex.Title != "" {
						label = fmt.Sprintf("example %q", ex.Title)
					}
					errs = append(errs, fmt.Sprintf("%s: action %s %s fails schema: %s", ctx, actionName, label, res.Error))
				}
			}
		}
		for metaName := range d.ActionMeta {
			if _, ok := d.Actions[metaName]; !ok {
				errs = append(errs, fmt.Sprintf("%s: ActionMeta names %q but no handler is registered", ctx, metaName))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("tool registry has %d validation error(s):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return nil
}
