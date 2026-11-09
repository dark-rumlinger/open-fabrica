package snapshotbuild

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"openfabrica/analysis"
)

// LibraryRef is one external CDN library declared on the plan (plan.external_libraries),
// which the public renderer auto-injects. Surfaced so the model sees what's available
// without a get_plan call and never re-declares one.
type LibraryRef struct {
	Name string
	URL  string
}

// ModuleImport is the set of names one module pulls from a single specifier.
type ModuleImport struct {
	Spec  string
	Names []string
}

// ModuleContract is the export/import surface of one JS module, parsed from its body.
// Surfaced so the model keeps the cross-module symbol contract consistent: importing a name
// a sibling does not export (or exporting one the module never defines) throws the moment
// the page's module graph loads.
type ModuleContract struct {
	Path string
	Bare string // the module's own bare import specifier ("world.state"); empty on a basename collision. Set at assembly (depends on the whole module set), never cached.
	// Collides is why Bare is empty: another module answers to the same basename, so
	// neither is in the importmap and every bare import of that name throws at load.
	// Rendered explicitly - without it a colliding module reads exactly like one that
	// simply has no bare form, and the snapshot would go on listing an unreachable
	// module as if a sibling could import it. Set at assembly, never cached.
	Collides bool
	Exports  []string            // named exports (the default export is omitted)
	Sigs     map[string][]string // exported function name -> its parameter tokens; lets a caller pass the right arguments (count and shape) without re-reading the definition. A nil/absent entry means the export is not a function (or its signature was not statically detectable).
	Imports  []ModuleImport      // project-internal imports, bare or relative (the cross-module contract). In the parse cache this holds EVERY parsed import; assembly filters out external-library specifiers.
	// Shapes is the DATA contract: an exported object literal (or a factory's returned
	// literal) mapped to its top-level field keys, the shape a sibling reads/writes across
	// the module boundary. The symbol contract proves a name exists and links; Shapes carries
	// the field set producer and consumer must agree on, so a `{yaw}` written / `.y` read
	// drift is visible here, not only at runtime. Keys only; absent when no object literal.
	Shapes map[string][]string
}

// loadDeclaredLibraries reads external_libraries from the most recent plan row so the
// snapshot surfaces already-declared CDN libraries (auto-injected by the renderer) as
// ground truth. Returns nil on a missing/malformed row, which renders no section.
func loadDeclaredLibraries(ctx context.Context, db *sql.DB) []LibraryRef {
	var planJSON string
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON); err != nil || planJSON == "" {
		return nil
	}
	var plan struct {
		ExternalLibraries []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil
	}
	var out []LibraryRef
	for _, l := range plan.ExternalLibraries {
		if strings.TrimSpace(l.Name) == "" {
			continue
		}
		out = append(out, LibraryRef{Name: l.Name, URL: l.URL})
	}
	return out
}

// buildModuleContracts parses every JS module's export and import surface for
// the snapshot's cross-module symbol contract. Two passes, because whether a
// bare import ("world.state") is project-internal depends on the whole module
// set: pass 1 parses each body (cached per path+body, every import kept raw);
// pass 2 resolves the set's specifier index and drops external-library
// specifiers - filtering in pass 2 keeps the (path, body) cache honest as the
// module set grows. A file with no named exports nor project-internal imports
// is skipped. Sorted by path.
func buildModuleContracts(ctx context.Context, db *sql.DB, cache *ModuleParseCache) []ModuleContract {
	rows, err := db.QueryContext(ctx, `
		SELECT path, COALESCE(body,'') FROM of_files
		WHERE kind = 'file' AND COALESCE(is_deleted,0) = 0
		  AND (path LIKE '%.js' OR path LIKE '%.mjs')
		ORDER BY path`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var parsed []ModuleContract
	var paths []string
	for rows.Next() {
		var p, body string
		if rows.Scan(&p, &body) != nil || p == "" {
			continue
		}
		paths = append(paths, p)
		if mc, ok := cache.get(p, body); ok {
			parsed = append(parsed, mc)
			continue
		}
		mc := ModuleContract{Path: p, Exports: analysis.ExtractNamedExports(body)}
		// Attach each exported function's parameter list, so the rendered contract shows
		// arity/shape and a caller passes the right arguments without re-reading the body.
		// Covers `export function f(...)` and `function f(...); export { f }`. First
		// definition wins; a non-function export gets no entry.
		if len(mc.Exports) > 0 {
			exported := make(map[string]bool, len(mc.Exports))
			for _, e := range mc.Exports {
				exported[e] = true
			}
			for _, fs := range analysis.JSFunctions(body) {
				if fs.Kind != "function" || !exported[fs.Name] {
					continue
				}
				if _, done := mc.Sigs[fs.Name]; done {
					continue
				}
				if mc.Sigs == nil {
					mc.Sigs = map[string][]string{}
				}
				mc.Sigs[fs.Name] = fs.Params
			}
		}
		bySpec := map[string][]string{}
		var specOrder []string
		for _, ni := range analysis.ExtractNamedImports(body) {
			if _, seen := bySpec[ni.Spec]; !seen {
				specOrder = append(specOrder, ni.Spec)
			}
			bySpec[ni.Spec] = append(bySpec[ni.Spec], ni.Imported)
		}
		for _, spec := range specOrder {
			mc.Imports = append(mc.Imports, ModuleImport{Spec: spec, Names: bySpec[spec]})
		}
		// Attach the data contract: the field keys of any exported object literal, so a
		// sibling wires to the real shape instead of guessing (the value analog of Sigs).
		mc.Shapes = analysis.ExtractExportedObjectShapes(body)
		cache.put(p, body, mc)
		parsed = append(parsed, mc)
	}
	if rows.Err() != nil {
		return nil
	}
	byName, collisions := analysis.ProjectModuleMap(paths)
	colliding := make(map[string]bool, len(collisions))
	for _, c := range collisions {
		colliding[c] = true
	}
	var out []ModuleContract
	for _, mc := range parsed {
		var kept []ModuleImport
		for _, im := range mc.Imports {
			if analysis.IsRelativeSpecifier(im.Spec) {
				kept = append(kept, im)
				continue
			}
			if _, ok := analysis.ResolveProjectModuleSpecifier(im.Spec, byName); ok {
				kept = append(kept, im)
			}
		}
		mc.Imports = kept
		if len(mc.Exports) == 0 && len(mc.Imports) == 0 {
			continue
		}
		if name := analysis.ProjectModuleName(mc.Path); name != "" {
			if byName[name] == mc.Path {
				mc.Bare = name
			} else if colliding[name] {
				mc.Collides = true
			}
		}
		out = append(out, mc)
	}
	return out
}
