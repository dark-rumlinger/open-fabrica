package content

// Read-only handlers shared by manage_files (get / outline / list) and
// manage_codebase (project_tree / grep). Both tools register these in their
// own init; the handlers themselves live here.

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/stringutil"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/project"
)

// matchPathGlob applies path.Match, whose separator handling is identical on
// every platform (filepath.Match made * separator-sensitive per OS).
func matchPathGlob(glob, p string) (bool, error) { return path.Match(glob, p) }

// getBodyBudget caps the numbered body a single get returns, in bytes. It is the
// shared store.MaxSingleReadBody so a `get` page is never smaller than the
// pre-inline per-file cap: a file too big to pre-inline is then still
// readable in one get instead of several paginated ones. It sits below
// manage_files' MaxResult so the JSON envelope + metadata fields still fit and
// the generic pipeline truncator never mangles a get result mid-line. When the
// window exceeds this, get paginates on a line boundary and hands back a
// next_from_line cursor instead.
const getBodyBudget = store.MaxSingleReadBody

// outlineTrigger is the raw body size, in bytes, above which a bare get (no
// from_line/to_line) returns the structural outline instead of the body. It is
// deliberately LOWER than getBodyBudget: the budget is measured on the
// line-NUMBERED body, so a raw body just under the budget (e.g. 21KB of dense
// JS) would otherwise be served in full and then truncated mid-file by the
// gutter overhead - no outline AND no tail. Keeping the trigger at 16KB removes that
// no-outline-but-truncated zone and pushes the model to range-reads earlier.
// Decoupled from store.MaxSingleReadBody on purpose: that constant also anchors
// the save cap (MaxSingleSaveBody = 2x) and the read pagination budget.
const outlineTrigger = 16 * 1024

// artifactActionGetImpl reads an artifact body. When fresh is true it bypasses
// the within-session read dedup and re-serves the full requested window even if
// that span was already delivered - the model's explicit "I lost my copy / I need
// to re-anchor" override for the case the residence tracker still believes the
// body is in context (the get-then-stub friction that otherwise costs iterations
// when an edit anchor drifts). The body is re-recorded as delivered on the way
// out, so a later dedup picks up from the fresh delivery.
func artifactActionGetImpl(ctx *tools.ToolContext, act *ArtGet) (*tools.Result, error) {
	kind, path, fromLine, toLine, fresh := act.Kind, act.FilePath, act.FromLine, act.ToLine, act.Fresh
	kind, path, errR := resolveKindPath(ctx, kind, path)
	if errR != nil {
		return errR, nil
	}
	a, err := store.ArtifactGet(ctx.DB, kind, path)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if a == nil {
		return tools.NotFound(kind, path), nil
	}
	totalLines := lineCount(a.Body)

	// Resolve the requested window. from_line defaults to 1, to_line to EOF.
	from := 1
	if fromLine > 0 {
		from = fromLine
	}
	toArg := toLine
	toSet := toLine != 0
	to := toArg
	if to <= 0 || to > totalLines {
		to = totalLines
	}
	// A window that starts past the end, or ends before it starts, has no lines: say
	// so with the file's length instead of slicing out of range or returning an empty
	// body that reads like an empty file.
	if totalLines > 0 && from > totalLines {
		return tools.FailCode(tools.CodeBadArg, "from_line %d is past the end of %s, which has %d lines.", from, path, totalLines), nil
	}
	if toSet && toLine > 0 && toLine < from {
		return tools.FailCode(tools.CodeBadArg, "to_line %d is before from_line %d; pass a range with from_line <= to_line (%s has %d lines).", toLine, from, path, totalLines), nil
	}

	// Within-session read dedup: once a line window has been delivered this session
	// (a full get, a paginated page, or a body inlined into a pass seed and pre-marked
	// by the build loop), the NEXT read of the same unchanged window returns a stub
	// instead of re-sending bytes the model already holds - the main lever against
	// input-token bloat from re-reading a module several times in one build. It keys
	// on the RANGE delivered, not just whole-body delivery: a file too large for one
	// read page is never delivered complete in one get, so a full-delivery-only dedup
	// never fired for exactly the big files that drive the worst re-read spirals. The
	// fingerprint hashes body content, so any body change invalidates the cache and the
	// next read serves the post-edit body in full, while a metadata-only touch (link,
	// scope, rename) leaves the dedup intact.
	fullRead := from <= 1 && (!toSet || toArg <= 0 || toArg >= totalLines)
	cacheKey := tools.ReadCacheKey(kind, path)
	fingerprint := tools.ReadFingerprint(a.Body)
	if !fresh && ctx.ReadTracker.ShouldStubRange(cacheKey, fingerprint, from, to) {
		return tools.Ok(map[string]any{
			"kind":        kind,
			"file_path":   path,
			"total_lines": totalLines,
			"from_line":   from,
			"to_line":     to,
			"updated_at":  a.UpdatedAt,
			"unchanged":   true,
			"note":        fmt.Sprintf("Lines %d-%d were already shown to you this session - in a prior get or an edit echo - and are byte-identical now, so the body is omitted to save context. Edit your existing copy in place with action=patch. If that copy is no longer in your context (an edit anchor will not match), re-request with fresh=true to force the current body to be re-served in full.", from, to),
		}), nil
	}

	bareGet := fromLine <= 0 && toLine <= 0

	// Outline-first for a bare get of a large file. When the model asks for no
	// explicit range and the body is over outlineTrigger, return the
	// structural OUTLINE (its map of exports / signatures / selectors) instead of the
	// first window. The model then asks for the exact range it needs, far cheaper
	// than paging a big file top-to-bottom, and on a provider without prefix caching
	// the map - not the whole body - is what rides each subsequent turn. The body
	// has NOT gone out, so this is not recorded as delivered; an explicit
	// from_line/to_line takes the normal ranged path below.
	if bareGet && len(a.Body) > outlineTrigger {
		outline := analysis.GenerateOutline(a.Body, analysis.FileTypeForOutline(path, a.ContentType))
		if outline != "" && len(outline) < len(a.Body) {
			return tools.Ok(map[string]any{
				"kind":         kind,
				"file_path":    path,
				"content_type": a.ContentType,
				"scope":        a.Scope,
				"status":       a.Status,
				"size":         a.Size,
				"total_lines":  totalLines,
				"updated_at":   a.UpdatedAt,
				"version":      store.ArtifactCurrentVersion(ctx.DB, kind, path),
				"outline":      outline,
				"note":         fmt.Sprintf("This file is %d lines - showing its structural OUTLINE (exports / signatures / selectors), not the full body, to keep context small. Wire against this surface. To read the actual source, call get again with from_line/to_line for the range you want (e.g. from_line=1 to read from the top). Edit in place with patch.", totalLines),
			}), nil
		}
	}

	body := sliceLines(a.Body, from, to)
	numbered := numberLines(body, from)

	// Page on a line boundary if the numbered window blows the budget, so the
	// generic byte truncator never fires and the model gets a precise cursor.
	shown, kept := trimToByteBudget(numbered, getBodyBudget)
	effTo := from + kept - 1
	if effTo > totalLines {
		effTo = totalLines
	}

	data := map[string]any{
		"kind":         kind,
		"file_path":    path,
		"content_type": a.ContentType,
		"metadata":     a.Metadata,
		"scope":        a.Scope,
		"status":       a.Status,
		"size":         a.Size,
		"total_lines":  totalLines,
		"updated_at":   a.UpdatedAt,
		"version":      store.ArtifactCurrentVersion(ctx.DB, kind, path),
		"from_line":    from,
		"to_line":      effTo,
		"body":         shown,
		"note":         "Line-number gutter ('N| ') is display-only - do not include it in patch search text.",
	}
	// Record the window actually delivered so a later read of the same span is
	// stubbed. A complete untruncated full read records the whole body (the fast
	// `full` path that also satisfies AlreadyDelivered for pre-inline callers); a
	// paginated page records just its [from,effTo] span, which accumulates as the
	// model pages forward.
	if effTo < to {
		next := effTo + 1
		data["truncated"] = true
		data["next_from_line"] = next
		data["hint"] = fmt.Sprintf("showed lines %d-%d of %d; call get with from_line=%d to continue, or action=outline for a structural map.", from, effTo, totalLines, next)
		ctx.ReadTracker.RecordDeliveredRange(cacheKey, fingerprint, from, effTo)
	} else if fullRead {
		ctx.ReadTracker.RecordDelivered(cacheKey, fingerprint)
	} else {
		ctx.ReadTracker.RecordDeliveredRange(cacheKey, fingerprint, from, effTo)
	}
	return tools.Ok(data), nil
}

func artifactActionOutline(ctx *tools.ToolContext, kind, path string) (*tools.Result, error) {
	kind, path, errR := resolveKindPath(ctx, kind, path)
	if errR != nil {
		return errR, nil
	}
	a, err := store.ArtifactGet(ctx.DB, kind, path)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if a == nil {
		return tools.NotFound(kind, path), nil
	}
	outline := analysis.GenerateOutline(a.Body, analysis.FileTypeForOutline(path, a.ContentType))
	return tools.Ok(map[string]any{
		"kind": kind, "file_path": path, "outline": outline,
		"version": store.ArtifactCurrentVersion(ctx.DB, kind, path),
	}), nil
}

func artifactActionProjectTree(ctx *tools.ToolContext, compact bool) (*tools.Result, error) {
	opts := project.ProjectTreeOpts{
		Compact: compact,
	}
	tree, err := project.BuildProjectTree(ctx.DB, opts)
	if err != nil {
		return tools.FailErr(err), nil
	}
	return tools.Ok(map[string]any{
		"tree": tree,
	}), nil
}

func artifactActionList(ctx *tools.ToolContext, kind string) (*tools.Result, error) {
	// list has no LLM-facing limit arg; keep the prior default.
	limit := 100
	items, err := store.ArtifactList(ctx.DB, store.ArtifactListOpts{Kind: kind, Limit: limit + 1})
	if err != nil {
		return tools.FailErr(err), nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		out = append(out, map[string]any{
			"kind":         a.Kind,
			"file_path":    a.Path,
			"content_type": a.ContentType,
			"scope":        a.Scope,
			"status":       a.Status,
			"size":         a.Size,
			"updated_at":   a.UpdatedAt,
		})
	}
	data := map[string]any{"artifacts": out, "count": len(out)}
	if len(out) > limit {
		data["artifacts"] = out[:limit]
		data["count"] = limit
		data["truncated"] = true
		data["note"] = fmt.Sprintf("showing the first %d; pass kind to narrow, or use manage_codebase project_tree for the whole project", limit)
	}
	return tools.Ok(data), nil
}

// artifactActionGrep streams every inline-stored artifact body and returns
// regex matches with line numbers and snippets. Disk-stored artifacts (binary
// blobs) are skipped, grep is text-only. Cheap full-table scan; fine for
// project-sized DBs.
// artifactActionGrep is the thin LLM-facing entrypoint: CodeGrep exposes only
// pattern + path_glob, so the scope/limit knobs take their prior defaults.
func artifactActionGrep(ctx *tools.ToolContext, pattern, pathGlob string) (*tools.Result, error) {
	return artifactGrep(ctx, pattern, "", "", pathGlob, 50, 5, 0)
}

// artifactGrep runs the regex scan with the full knob set. Kept separate so the
// internal behaviour (path_prefix, kind filter, limit, max_per_file,
// context_lines) stays unit-testable even though the tool surface only forwards
// pattern + path_glob.
func artifactGrep(ctx *tools.ToolContext, pattern, kindFilter, pathPrefix, pathGlob string, limit, maxPerFile, contextLines int) (*tools.Result, error) {
	if pattern == "" {
		return tools.Fail("pattern is required (Go-syntax regex; use (?i) inline for case-insensitive)"), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return tools.Fail("invalid regex: %v, escape literal regex metacharacters or check syntax at https://pkg.go.dev/regexp/syntax", err), nil
	}
	if pathGlob != "" {
		if _, err := matchPathGlob(pathGlob, ""); err != nil {
			return tools.Fail("invalid path_glob: %v", err), nil
		}
	}
	if limit <= 0 {
		limit = 50
	}
	if maxPerFile <= 0 {
		maxPerFile = 5
	}
	// context_lines is capped at 5 because each extra line widens every match
	// row by ~240 chars and the result still carries a default 50-match limit;
	// without an upper bound a single call can blow past MaxResult.
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > 5 {
		contextLines = 5
	}

	q := `SELECT kind, path, body FROM of_files WHERE is_deleted = 0 AND storage = 'inline'`
	var qArgs []any
	if kindFilter != "" {
		q += ` AND kind = ?`
		qArgs = append(qArgs, kindFilter)
	}
	if pathPrefix != "" {
		q += ` AND path LIKE ?`
		qArgs = append(qArgs, pathPrefix+"%")
	}
	q += ` ORDER BY kind, path`
	rows, err := ctx.DB.Query(q, qArgs...)
	if err != nil {
		return tools.Fail("scanning artifacts: %v", err), nil
	}
	defer rows.Close()

	var matches []grepMatch
	filesHit := 0
	totalScanned := 0
	truncated := false

	for rows.Next() {
		if len(matches) >= limit {
			truncated = true
			break
		}
		var kind, path, body string
		if err := rows.Scan(&kind, &path, &body); err != nil {
			continue
		}
		// path_glob is applied post-scan because SQLite has no glob match
		// operator that maps cleanly to glob semantics. path_prefix
		// is already narrowed in SQL above; both filters AND-combine.
		if pathGlob != "" {
			ok, _ := matchPathGlob(pathGlob, path)
			if !ok {
				continue
			}
		}
		totalScanned++
		fileMatches := 0
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			if fileMatches >= maxPerFile {
				break
			}
			if !re.MatchString(line) {
				continue
			}
			snippet := buildGrepSnippet(lines, i, contextLines)
			matches = append(matches, grepMatch{
				Kind:    kind,
				Path:    path,
				Line:    i + 1,
				Snippet: snippet,
			})
			fileMatches++
			if len(matches) >= limit {
				truncated = true
				break
			}
		}
		if fileMatches > 0 {
			filesHit++
		}
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("scanning artifacts: %v", err), nil
	}

	data := map[string]any{
		"pattern":       pattern,
		"matches":       matches,
		"match_count":   len(matches),
		"files_hit":     filesHit,
		"files_scanned": totalScanned,
		"truncated":     truncated,
		"max_per_file":  maxPerFile,
		"context_lines": contextLines,
	}
	return tools.Ok(data), nil
}

// grepMatch is one row in artifactActionGrep's result. Hoisted to package
// scope (rather than declared inside the handler) so tests can construct and
// inspect typed values; the JSON tags drive the tool-result payload shape.
type grepMatch struct {
	Kind    string `json:"kind"`
	Path    string `json:"file_path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// buildGrepSnippet returns the snippet for one grep match: the matched line
// joined with up to contextLines neighbours on either side. Each line is
// truncated to 240 chars so a long match line with context can't blow the
// per-row payload budget. contextLines==0 returns just the matched line.
func buildGrepSnippet(lines []string, matchIdx, contextLines int) string {
	start := matchIdx - contextLines
	if start < 0 {
		start = 0
	}
	end := matchIdx + contextLines + 1
	if end > len(lines) {
		end = len(lines)
	}
	out := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		out = append(out, stringutil.Truncate(line, 240))
	}
	return strings.Join(out, "\n")
}
