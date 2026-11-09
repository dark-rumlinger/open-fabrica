package snapshotbuild

// ProjectSnapshot is the compact current state of a project, regenerated on
// every LLM turn from the database: the ground-truth "what already exists" view
// the LLM uses to stay coherent across tool calls. Mostly platform-agnostic; the
// one place it looks inside JS is the per-module export/import surface
// (ModuleContract), surfaced every turn so the LLM keeps imports and exports
// consistent without re-reading every sibling.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"sync"

	"openfabrica/store"
)

// ProjectSnapshot is everything the LLM needs to know about the current state
// of the project. Built fresh from the database on every LLM turn.
type ProjectSnapshot struct {
	Pages      []PageRef
	Layouts    []LayoutRef
	Components []ComponentRef
	Tables     []TableRef
	Views      []ViewRef
	Endpoints  []EndpointRef
	Files      []FileRef
	Libraries  []LibraryRef

	// UnlinkedPageFiles is the import-aware set of page-scope artifacts that are
	// genuinely orphaned: neither linked via of_page_assets nor reachable through
	// the ES-module import graph from a linked JS entry. Computed via
	// store.UnlinkedPageArtifacts so snapshot and out-of-loop verification agree.
	UnlinkedPageFiles []string

	// Modules is the per-module export/import surface for JS modules, the cross-module
	// symbol contract the model must keep consistent. See ModuleContract.
	Modules []ModuleContract

	// Styles is the design-system contract parsed from the global stylesheet(s): the tokens
	// and class names every page should reuse. Surfaced every turn so it survives a context
	// rollover and pages stay visually consistent. See StyleContract.
	Styles []StyleContract

	// EffectiveCSP is the Content-Security-Policy this project's public pages are
	// actually served under (config.ResolveCSP). Set only by the caller that feeds
	// the model prompt; other callers leave it empty and render nothing. The render
	// stays silent on the permissive shipped default, so a project that is not
	// constrained pays no bytes for the fact.
	EffectiveCSP string

	// UserFlowsOff is set when the owner turned user-flow tests off; the render says so only then.
	UserFlowsOff bool

	// PageStyles is the same names-only surface for each page's PAGE-SCOPE
	// stylesheet(s), kept separate from Styles so a page one-off is never listed
	// under "reuse on every page" while still answering "does this page's CSS
	// already have `.error`?" without a re-read. See PageStyleContract.
	PageStyles []PageStyleContract

	// PageDOM is the per-page element-id surface (page markup + shared layout +
	// included components), limited to pages that carry page-scope JS - the DOM
	// contract a page-scope script must bind to rather than invent. See DOMContract.
	PageDOM []DOMContract

	// Entities maps a stable per-entity key ("page:/posts", "module:js/app.js", ...) to a
	// short hash of that entity's fingerprint-relevant fields. The whole-snapshot Fingerprint
	// is derived from it (one source of truth), and the tool loop diffs it against the session
	// baseline to show the model what changed. Populated by BuildProjectSnapshot.
	Entities map[string]string

	// prevModuleDetails indexes the previous snapshot's module detail lines by
	// path. Set only on a delta subset (RenderDelta), so the module loop can
	// collapse lines the model already has verbatim. Nil renders full records.
	prevModuleDetails map[string][]string
}

// ModuleParseCache memoizes parsed module AND style contracts across snapshot rebuilds
// within one build, keyed by path + a hash of the body, so an unchanged JS module or CSS
// stylesheet is parsed once instead of on every mutating turn. Build-scoped on the worker
// and wiped by resetBuildState. A nil *ModuleParseCache is inert: every method is safe and
// reports a miss, so out-of-loop callers pass nil for a cold parse.
type ModuleParseCache struct {
	mu      sync.Mutex
	entries map[string]ModuleContract
	styles  map[string]StyleContract
}

// NewModuleParseCache returns a ready cache.
func NewModuleParseCache() *ModuleParseCache {
	return &ModuleParseCache{
		entries: map[string]ModuleContract{},
		styles:  map[string]StyleContract{},
	}
}

func moduleParseKey(path, body string) string {
	sum := sha256.Sum256([]byte(body))
	return path + "\x00" + hex.EncodeToString(sum[:12])
}

// get returns the cached contract for (path, body) and whether it was present.
func (c *ModuleParseCache) get(path, body string) (ModuleContract, bool) {
	if c == nil {
		return ModuleContract{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mc, ok := c.entries[moduleParseKey(path, body)]
	return mc, ok
}

// put records the parsed contract for (path, body).
func (c *ModuleParseCache) put(path, body string, mc ModuleContract) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[moduleParseKey(path, body)] = mc
}

// getStyle returns the cached style contract for (path, body) and whether it was
// present. A cached zero contract (a CSS file with no surface) is a valid hit.
func (c *ModuleParseCache) getStyle(path, body string) (StyleContract, bool) {
	if c == nil {
		return StyleContract{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc, ok := c.styles[moduleParseKey(path, body)]
	return sc, ok
}

// putStyle records the parsed style contract for (path, body).
func (c *ModuleParseCache) putStyle(path, body string, sc StyleContract) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.styles[moduleParseKey(path, body)] = sc
}

// BuildProjectSnapshot reads the current state of the project from the project database and
// returns a ProjectSnapshot. Cheap: indexed reads only. parseCache (optional; nil for a
// cold parse) memoizes module-contract parsing across the many rebuilds one build performs.
func BuildProjectSnapshot(ctx context.Context, db *sql.DB, parseCache *ModuleParseCache) (*ProjectSnapshot, error) {
	s := &ProjectSnapshot{}

	s.Pages = loadPages(ctx, db)
	s.Tables = loadTables(ctx, db)

	s.Views = loadViews(ctx, db)

	s.Endpoints = loadEndpoints(ctx, db)

	s.Layouts = loadLayouts(ctx, db)
	s.Components = loadComponents(ctx, db)
	s.Files = loadFiles(ctx, db)

	// Compute the orphan set with the same import-graph traversal the post-build probe uses,
	// so a page-scope module reached transitively from a linked entry is never falsely
	// flagged. store.UnlinkedPageArtifacts keeps snapshot and out-of-loop verification consistent.
	s.UnlinkedPageFiles = store.UnlinkedPageArtifacts(db)

	s.Modules = buildModuleContracts(ctx, db, parseCache)

	s.Styles = buildStyleContracts(ctx, db, parseCache)

	// The DOM contract matters only for pages a page-scope script wires against.
	// The link alone is not that signal: modules are saved first and the entry
	// attached last, so keying on of_page_assets would withhold the contract
	// while the wiring is written. Use the link when one exists; fall back to
	// "the project has page-scope JS at all" while none does.
	jsPages := map[string]bool{}
	for _, p := range s.Pages {
		for _, a := range p.Assets {
			if strings.HasSuffix(a, ".js") {
				jsPages[p.Path] = true
				break
			}
		}
	}
	if len(jsPages) == 0 && hasPageScopeJS(s.Files) {
		for _, p := range s.Pages {
			jsPages[p.Path] = true
		}
	}
	s.PageDOM = buildDOMContracts(ctx, db, jsPages)

	s.PageStyles = buildPageStyleContracts(ctx, db, parseCache, s.Pages)

	s.Libraries = loadDeclaredLibraries(ctx, db)

	s.Entities = s.entityFingerprints()

	return s, nil
}
