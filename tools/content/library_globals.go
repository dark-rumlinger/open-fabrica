package content

// Which ambient globals this project's declared CDN libraries supply, for the
// save-time write gate.
//
// The tools layer cannot import the engine, so the plan's external_libraries are
// decoded straight into the dependency-free analysis shape - the same field names
// the engine's LibrarySpec marshals. The DECISION (which library can define a
// global, and whether the set is exact) lives in analysis.LibraryGlobals, shared
// with the round-level validator's unboundRefSeverity so the two gates cannot
// disagree about when an unbound name is knowable.

import (
	"encoding/json"

	"openfabrica/analysis"
	"openfabrica/tools"
)

// currentPlanLibrariesQuery reads the live plan document. Same row the engine writes
// at PLAN and manage_libraries upserts into; mirrors plan_title.go's lookup.
const currentPlanLibrariesQuery = `SELECT COALESCE(plan_json, '') FROM of_runs
	WHERE plan_json IS NOT NULL AND plan_json != ''
	ORDER BY started_at DESC, id DESC LIMIT 1`

// projectLibraryGlobals returns the ambient globals the project's declared classic
// libraries name (`QRCode`, `confetti`), exact or not: a named global is bound at runtime
// either way, so no gate may call it undefined. Empty when the plan is unreadable.
func projectLibraryGlobals(ctx *tools.ToolContext) map[string]bool {
	libs, ok := projectExternalLibs(ctx)
	if !ok {
		return nil
	}
	globals, _ := analysis.LibraryGlobals(libs)
	return globals
}

// projectExternalLibs reads the live plan's declared external libraries.
// ok=false is the safe answer and is what an absent, unreadable or unparseable
// plan returns: with no authoritative declaration list, gates must not block on
// an unbound name a library the platform cannot see might be supplying.
func projectExternalLibs(ctx *tools.ToolContext) (libs []analysis.ExternalLib, ok bool) {
	if ctx == nil || ctx.DB == nil {
		return nil, false
	}
	var planJSON string
	if err := ctx.DB.QueryRow(currentPlanLibrariesQuery).Scan(&planJSON); err != nil || planJSON == "" {
		return nil, false
	}
	var plan struct {
		ExternalLibraries []struct {
			Name       string `json:"name"`
			URL        string `json:"url"`
			Kind       string `json:"kind"`
			ModuleName string `json:"module_name"`
			GlobalName string `json:"global_name"`
		} `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, false
	}
	libs = make([]analysis.ExternalLib, 0, len(plan.ExternalLibraries))
	for _, l := range plan.ExternalLibraries {
		libs = append(libs, analysis.ExternalLib{
			Name: l.Name, URL: l.URL, Kind: l.Kind,
			ModuleName: l.ModuleName, GlobalName: l.GlobalName,
		})
	}
	return libs, true
}
