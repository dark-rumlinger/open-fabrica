package content

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// resolve / where_used: the symbol-index read surface on manage_codebase. They
// turn "grep then read three files to find where to edit" into one indexed
// lookup. Both self-heal a pre-index project by lazily backfilling a bounded
// batch of stale files before querying (store.BackfillStaleSymbols), so a project
// that predates the index converges over a few calls with no migration data pass.

// backfillBudget caps how many stale files a single lookup reindexes, so the
// first lookup on a large pre-existing project stays cheap and the index fills in
// over subsequent calls rather than all at once.
const backfillBudget = 25

type symbolDef struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FileKind  string `json:"file_kind"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
}

// artifactActionResolve returns every indexed definition of a symbol name (each
// with file:line + signature), the files that reference it, and a structural
// ranking: exported defs first, then most-recently-updated. A name that is not an
// indexed symbol returns success with an empty result and a grep hint, so a miss
// is never a dead end.
func artifactActionResolve(ctx *tools.ToolContext, symbol, kindFilter string) (*tools.Result, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return tools.Fail("symbol is required"), nil
	}
	_, warming, _ := store.BackfillStaleSymbols(ctx.DB, backfillBudget)

	q := `SELECT name, kind, file_kind, file_path, line, signature, exported
		FROM of_symbols WHERE name = ?`
	args := []any{symbol}
	if kindFilter != "" {
		q += ` AND kind = ?`
		args = append(args, kindFilter)
	}
	// Exported defs first, then newest - the structural ranking. SQLite sorts the
	// boolean (1/0) descending so exported lands on top.
	q += ` ORDER BY exported DESC, updated_at DESC, file_path`

	rows, err := ctx.DB.Query(q, args...)
	if err != nil {
		return tools.Fail("querying symbols: %v", err), nil
	}
	defer rows.Close()

	var defs []symbolDef
	for rows.Next() {
		var d symbolDef
		var exported int
		if err := rows.Scan(&d.Name, &d.Kind, &d.FileKind, &d.FilePath, &d.Line, &d.Signature, &exported); err != nil {
			continue
		}
		d.Exported = exported == 1
		defs = append(defs, d)
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("querying symbols: %v", err), nil
	}

	referencedBy := distinctReferencingFiles(ctx, symbol)

	if len(defs) == 0 {
		return tools.Ok(withWarmingHint(map[string]any{
			"symbol":        symbol,
			"match_count":   0,
			"definitions":   []symbolDef{},
			"referenced_by": referencedBy,
			"hint":          "no indexed symbol by that name - try manage_codebase action=grep for a full-text scan, or check the spelling",
		}, warming)), nil
	}

	return tools.Ok(withWarmingHint(map[string]any{
		"symbol":          symbol,
		"match_count":     len(defs),
		"definitions":     defs,
		"referenced_by":   referencedBy,
		"reference_count": len(referencedBy),
	}, warming)), nil
}

// withWarmingHint annotates a lookup result when the symbol index is still
// backfilling: a non-zero count of stale files means the query ran against a
// partial index, so a thin or empty result may be incomplete and the caller
// should rerun after the index catches up. It is a no-op when nothing is pending.
func withWarmingHint(data map[string]any, warming int) map[string]any {
	if warming > 0 {
		data["index_warming"] = fmt.Sprintf("symbol index still warming: %d file(s) not yet indexed - results may be incomplete, rerun to converge", warming)
	}
	return data
}

// distinctReferencingFiles returns the sorted, deduplicated list of files that
// reference symbol via an indexed edge (import).
func distinctReferencingFiles(ctx *tools.ToolContext, symbol string) []string {
	rows, err := ctx.DB.Query(`SELECT DISTINCT ref_file_path FROM of_symbol_refs
		WHERE symbol_name = ? ORDER BY ref_file_path`, symbol)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	_ = rows.Err() // best-effort; a partial reference list is fine
	return out
}

type whereUsedRef struct {
	Line    int    `json:"line"`
	RefKind string `json:"ref_kind"`
}

type whereUsedFile struct {
	FileKind string         `json:"file_kind"`
	FilePath string         `json:"file_path"`
	Refs     []whereUsedRef `json:"refs"`
}

// artifactActionWhereUsed lists every reference to a symbol grouped by the file
// that makes it, from the symbol index.
func artifactActionWhereUsed(ctx *tools.ToolContext, symbol string) (*tools.Result, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return tools.Fail("symbol is required"), nil
	}
	_, warming, _ := store.BackfillStaleSymbols(ctx.DB, backfillBudget)

	rows, err := ctx.DB.Query(`SELECT ref_file_kind, ref_file_path, ref_line, ref_kind
		FROM of_symbol_refs WHERE symbol_name = ? ORDER BY ref_file_path, ref_line`, symbol)
	if err != nil {
		return tools.Fail("querying references: %v", err), nil
	}
	defer rows.Close()

	byFile := map[string]*whereUsedFile{}
	order := []string{}
	total := 0
	for rows.Next() {
		var fkind, fpath, rkind string
		var line int
		if err := rows.Scan(&fkind, &fpath, &line, &rkind); err != nil {
			continue
		}
		key := fkind + "\x00" + fpath
		f, ok := byFile[key]
		if !ok {
			f = &whereUsedFile{FileKind: fkind, FilePath: fpath}
			byFile[key] = f
			order = append(order, key)
		}
		f.Refs = append(f.Refs, whereUsedRef{Line: line, RefKind: rkind})
		total++
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("querying references: %v", err), nil
	}

	sort.Strings(order)
	files := make([]whereUsedFile, 0, len(order))
	for _, key := range order {
		files = append(files, *byFile[key])
	}

	return tools.Ok(withWarmingHint(map[string]any{
		"symbol":          symbol,
		"reference_count": total,
		"files":           files,
	}, warming)), nil
}
