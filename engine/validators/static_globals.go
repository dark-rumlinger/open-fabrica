package validators

// Whether a member/assign undefined_reference is DETERMINISTIC for this project,
// and which ambient globals the declared libraries supply.
//
// The deciding question: could a declared library be supplying the name as an
// ambient global? A classic UMD/IIFE `<script src>` defines one, so a library
// that names it (LibrarySpec.GlobalName) is ALLOWLISTED and every other unbound
// name is genuinely undefined - the check gates at error. A classic library that
// names nothing leaves the platform unable to tell its global from a typo, so the
// whole project falls back to warning: gating at error there would fix-pass
// against working code. Conservative: any doubt keeps warning severity.
//
// The classification itself lives in analysis.LibraryGlobals, shared with the
// save-time write gate so the two cannot drift apart.

import (
	"database/sql"
	"encoding/json"

	"openfabrica/analysis"
	"openfabrica/engine"
	"openfabrica/store"
)

// planLibraryGlobals reads the current plan's declared libraries and returns the
// ambient globals they supply plus whether that set is exact. Unreadable plan ->
// the safe answer (no globals known, not exact), which keeps the check
// non-blocking rather than blocking on a name a library might own.
func planLibraryGlobals(pdb *sql.DB) (globals map[string]bool, exact bool) {
	if pdb == nil {
		return nil, false
	}
	var planJSON string
	if err := pdb.QueryRow(`SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON); err != nil || planJSON == "" {
		return nil, false
	}
	var plan struct {
		ExternalLibraries []LibrarySpec `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, false
	}
	return analysis.LibraryGlobals(engine.ExternalLibs(plan.ExternalLibraries))
}

// unboundRefSeverity picks the severity for the member/assign undefined_reference check
// and returns the library globals to allowlist while running it. Deterministic (and so
// fix-pass-driving; the category never blocks a deploy, see blockingCategories) exactly
// when every globals-capable library named the global it defines, so no undeclared name
// can be one.
func unboundRefSeverity(pdb *sql.DB) (sev string, deterministic bool, libGlobals map[string]bool) {
	globals, exact := planLibraryGlobals(pdb)
	if !exact {
		return store.SeverityWarning, false, globals
	}
	return store.SeverityError, true, globals
}
