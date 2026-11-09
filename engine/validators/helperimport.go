package validators

// The platform-helper import validator: catches a page importing 'ofnet',
// 'e2ee', or 'auth' when that helper was never emitted into the project (the
// platform writes them as a side effect of creating the endpoint they belong
// to). Import one that is absent and NOTHING else complains - the failure is
// "Failed to resolve module specifier" in the browser console at runtime, which
// no build signal ever pointed at.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"openfabrica/analysis"
)

type helperImportValidator struct{}

func (helperImportValidator) Name() string { return "helper_import_missing" }

// helperEmitHint names the tool call that writes each helper, so the fix is the
// next action rather than a search.
var helperEmitHint = map[string]string{
	"ofnet": "create a realtime endpoint (manage_relay / manage_tick / manage_authoritative / manage_matchmaking / manage_signaling) - every one of them emits it",
	"e2ee":  "create the realtime endpoint with e2ee=true (for a call/signaling endpoint use `manage_signaling action=update endpoint_path=<p> e2ee=true`, which also turns on room_gated; for a chat relay use `manage_relay action=update endpoint_path=<p> e2ee=true`) - the helper is only emitted for an encrypted endpoint",
	"auth":  "run `manage_auth action=create` - the auth helper is emitted with the auth endpoint",
}

// bareImportRe matches an import/export whose source is a bare specifier, in the
// forms the model actually writes: static `from 'x'`, side-effect `import 'x'`,
// and dynamic `import('x')`. A specifier built from a variable is invisible, the
// acceptable false-negative.
var bareImportRe = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*)['"]([A-Za-z0-9_@/.-]+)['"]`)

func (helperImportValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	reserved := analysis.ReservedModulePaths()

	// Which helpers exist. A read error reports nothing rather than accusing every
	// import of being unresolvable.
	present := map[string]bool{}
	rows, err := pdb.Query(`SELECT path FROM of_files WHERE is_deleted = 0 AND scope = 'global'`)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			present[strings.ToLower(p)] = true
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		return nil
	}

	// One issue per (file, specifier): the same missing helper imported from six
	// leaves is one fix, and six copies of it buries everything else.
	seen := map[string]bool{}
	var issues []Issue
	for _, f := range jsAndPageFiles(pdb) {
		for _, m := range bareImportRe.FindAllStringSubmatch(f.body, -1) {
			spec := m[1]
			target, isHelper := reserved[spec]
			if !isHelper || present[strings.ToLower(target)] {
				continue
			}
			key := f.path + "\x00" + spec
			if seen[key] {
				continue
			}
			seen[key] = true
			issues = append(issues, Issue{
				Severity:      "error",
				Category:      "helper_import_missing",
				File:          f.path,
				Deterministic: true,
				Message: fmt.Sprintf("`%s` imports the platform helper `%s`, but `%s` does not exist in this project, so the browser fails on "+
					"\"Failed to resolve module specifier\" and the page's script never runs. The helper is not a file you author - the platform emits it when you create "+
					"the endpoint it belongs to. To get it: %s. (Do not write your own %s module: the specifier is reserved and a user file cannot claim it.)",
					f.path, spec, target, helperEmitHint[spec], spec),
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].File != issues[j].File {
			return issues[i].File < issues[j].File
		}
		return issues[i].Message < issues[j].Message
	})
	return issues
}

func init() {
	RegisterValidator(helperImportValidator{})
}
