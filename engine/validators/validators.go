// Package validators holds the engine's post-build validators. Each file is one
// validator: an empty struct with Name/Validate and an init() that registers it -
// adding a validator is adding a file, the build loop never changes. main.go
// blank-imports the package (the same pattern as the tool packages). The fix-loop
// bookkeeping that CONSUMES these issues (triage, demotion, fingerprint dedup) stays
// in the engine package; the save-time write-path twins live in tools/content + store.
package validators

import (
	"database/sql"
	"strings"

	"openfabrica/engine"
)

// Issue and RegisterValidator alias the engine's contract so the per-validator
// files read the same as when they lived in the engine package.
type Issue = engine.Issue

func RegisterValidator(v engine.Validator) { engine.RegisterValidator(v) }

// Plan and the related plan-shaped types the decision-conformance and PWA
// validators read are aliased here so the moved files keep reading as they did
// in the engine package.
type Plan = engine.Plan
type LibrarySpec = engine.LibrarySpec
type DecisionCheck = engine.DecisionCheck
type PlanTaskSpec = engine.PlanTaskSpec
type PlanDecision = engine.PlanDecision

// ParsePlan aliases the engine's plan parser (see engine.ParsePlan).
var ParsePlan = engine.ParsePlan

// Shared project-content queries. Several validators scan the same slices of
// of_files; these helpers are the one home for those queries so the SQL (and
// its is_deleted / kind / platform-managed rules) cannot drift between
// validators.

// bodyRow is one of_files row a validator scans: path, body, and the raw
// metadata JSON (empty string when null).
type bodyRow struct {
	path, body, meta string
}

// jsAndPageFiles returns every live page artifact and .js/.mjs file - the
// slice of client-side code the raw-WebSocket and API-target scans walk.
// Rows with an empty body and platform-managed helper files are skipped.
// Returns nil on a read error so the caller self-skips rather than guessing.
func jsAndPageFiles(pdb *sql.DB) []bodyRow {
	rows, err := pdb.Query(`SELECT path, COALESCE(body,''), COALESCE(metadata,'')
		FROM of_files
		WHERE is_deleted = 0 AND kind <> 'test' AND (kind = 'page' OR path LIKE '%.js' OR path LIKE '%.mjs')`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []bodyRow
	for rows.Next() {
		var r bodyRow
		if rows.Scan(&r.path, &r.body, &r.meta) != nil || r.body == "" {
			continue
		}
		if platformManaged(r.meta) {
			continue
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil // partial read: self-skip, the write-path gates own read errors
	}
	return out
}

// layoutBodies returns every live layout keyed by its lowercased path.
// Returns nil on a query error so the caller self-skips rather than guessing.
func layoutBodies(pdb *sql.DB) map[string]string {
	rows, err := pdb.Query("SELECT path, COALESCE(body,'') FROM of_files WHERE kind = 'layout' AND is_deleted = 0")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, body string
		if rows.Scan(&path, &body) == nil && path != "" {
			out[strings.ToLower(path)] = body
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}
