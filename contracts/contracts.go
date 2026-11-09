// Package contracts is the single source of truth for the load-bearing FACTS the
// LLM must reproduce verbatim (the REST response envelope, auth token key, E2EE
// wire envelope, realtime frames, page/layout and routing contracts, encrypted-app
// security model). Each fact is defined ONCE here as an embedded markdown block and
// transcluded into stage prompts and packs via the `{{contract:<id>}}` sentinel
// (see the pack assembler), instead of being restated - and drifting - across
// guides, recipes, and Go strings.
//
// A contract optionally declares the doc slugs it `governs` plus the short
// `probes` (canonical tokens) that MUST appear in every governed doc; a
// consistency test asserts that, so a silently diverging copy fails the build.
// Load/Render are pure and deterministic (the prompt-cache contract).
package contracts

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"openfabrica/internal/frontmatter"
	"openfabrica/knowledge"
)

// Contract IDs. Every id string lives here ONCE; Go references a contract through
// its constant (never a bare literal), so a renamed contract file breaks at the
// reference instead of silently rendering nothing. The markdown {{contract:id}}
// tokens are validated separately by the consistency tests.
const (
	IDAPIResponse       = "api-response"
	IDEndpointRoutes    = "endpoint-routes"
	IDAuthWindowAPI     = "auth-window-api"
	IDE2EEWire          = "e2ee-wire"
	IDE2EESecurityModel = "e2ee-security-model"
	IDRealtimeFrames    = "realtime-frames"
	IDPageLayout        = "page-layout"
	IDRouting           = "routing"
	IDTriggerEvents     = "trigger-events"
	IDPaymentFlow       = "payment-flow"
	IDWebRTC            = "webrtc"
	IDTrustBoundary     = "trust-boundary"
	IDRoomReducer       = "room-reducer"
	IDModuleScope       = "module-scope"
)

// AllIDs is every shipped contract id (the single home for the set). Adding a
// contract means adding its constant above and here; a test asserts each resolves.
var AllIDs = []string{
	IDAPIResponse, IDEndpointRoutes, IDAuthWindowAPI, IDE2EEWire, IDE2EESecurityModel,
	IDRealtimeFrames, IDRoomReducer, IDPageLayout, IDRouting, IDTriggerEvents,
	IDPaymentFlow, IDWebRTC, IDModuleScope, IDTrustBoundary,
}

// Contract is one canonical fact.
type Contract struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Kind is an authoring hint: json-literal | kv | wire-envelope | code | prose.
	Kind string `json:"kind"`
	// Governs names the doc slugs (guides/recipes) whose inline copy of this fact
	// must carry the Probes below. Empty = source-of-truth only (no doc restates
	// it yet; e.g. facts currently inlined in a stage prompt or in Go).
	Governs []string `json:"governs"`
	// Probes are the canonical tokens that must appear (normalized) in every
	// governed doc. Short and distinctive so Matches is robust.
	Probes []string `json:"probes"`
	// Body is the authoritative block rendered verbatim by Render into a prompt
	// or pack at assembly time.
	Body string `json:"body"`
}

// Load parses every embedded contract, keyed by ID. It errors on a malformed
// file or a duplicate ID so the consistency test catches drift.
//
// The embedded tree is fixed at runtime, so the parse is done once and cached.
// Each call returns a fresh copy of the map so a caller cannot mutate the cache.
func Load() (map[string]Contract, error) {
	contractsOnce.Do(func() {
		contractsCache, contractsErr = load()
	})
	if contractsErr != nil {
		return nil, contractsErr
	}
	out := make(map[string]Contract, len(contractsCache))
	for k, v := range contractsCache {
		out[k] = v
	}
	return out, nil
}

var (
	contractsOnce  sync.Once
	contractsCache map[string]Contract
	contractsErr   error
)

func load() (map[string]Contract, error) {
	entries, err := knowledge.FS.ReadDir("contracts")
	if err != nil {
		return nil, fmt.Errorf("reading embedded contracts: %w", err)
	}
	out := make(map[string]Contract)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".contract.md") {
			continue
		}
		raw, err := knowledge.FS.ReadFile("contracts/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading contracts/%s: %w", e.Name(), err)
		}
		c, err := parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("parsing contracts/%s: %w", e.Name(), err)
		}
		if _, dup := out[c.ID]; dup {
			return nil, fmt.Errorf("duplicate contract id %q (contracts/%s)", c.ID, e.Name())
		}
		out[c.ID] = c
	}
	return out, nil
}

// List returns every contract sorted by ID, for deterministic iteration.
func List() ([]Contract, error) {
	m, err := Load()
	if err != nil {
		return nil, err
	}
	out := make([]Contract, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Render returns the canonical block for id (trimmed), or "" if unknown. This is
// the value substituted for the `{{contract:id}}` sentinel - pure and stable, so
// transclusion stays byte-deterministic for the prompt cache.
func Render(id string) string {
	m, err := Load()
	if err != nil {
		return ""
	}
	c, ok := m[id]
	if !ok {
		return ""
	}
	return strings.TrimSpace(c.Body)
}

// MustRender is Render for a fixed, code-driven reference: it panics if id is
// unknown or renders empty, so a Go reference to a renamed/mistyped contract
// fails loudly at first call instead of silently inlining nothing. Always pass an
// ID* constant. (Dynamic {{contract:id}} tokens parsed from markdown use Render,
// which tolerates an unknown id so the consistency test can flag it.)
func MustRender(id string) string {
	if r := Render(id); r != "" {
		return r
	}
	panic(fmt.Sprintf("contracts.MustRender: unknown or empty contract id %q", id))
}

// Normalize lowercases and collapses all whitespace runs to single spaces, so a
// probe matches regardless of indentation or line wrapping.
func Normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Matches reports whether every one of the contract's probes appears (normalized)
// in text. A contract with no probes never claims a match (it governs nothing
// yet). Used by the consistency test to assert governed docs carry the fact.
func (c Contract) Matches(text string) bool {
	if len(c.Probes) == 0 {
		return false
	}
	n := Normalize(text)
	for _, p := range c.Probes {
		if !strings.Contains(n, Normalize(p)) {
			return false
		}
	}
	return true
}

// parse splits "---\nkey: value...\n---\n\nbody" front-matter.
func parse(raw string) (Contract, error) {
	var c Contract
	header, body, err := frontmatter.Split(raw)
	if err != nil {
		return c, err
	}
	c.Body = body

	for _, line := range strings.Split(header, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "id":
			c.ID = val
		case "title":
			c.Title = val
		case "kind":
			c.Kind = val
		case "governs":
			c.Governs = frontmatter.SplitList(val)
		case "probes":
			c.Probes = frontmatter.SplitList(val)
		}
	}
	if c.ID == "" || c.Body == "" {
		return c, fmt.Errorf("contract needs id and body")
	}
	return c, nil
}
