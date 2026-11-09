package observability

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_diagnostics is the read-only inspection surface: server health,
// recent errors, cross-artifact integrity, orphaned page assets. The active
// probes (test_endpoint, test_url, test_auth_flow, test_page, test_module,
// test_websocket) live in manage_testing - they back-call the public server, so they cost a
// real request each; reach for them to verify behaviour end-to-end (available
// in BUILD and LIVE) and stay on these read-only checks for "what's broken".

func init() {
	tools.Define(tools.Tool{
		Name:      "manage_diagnostics",
		Probe:     true,
		Category:  "observability",
		OneLiner:  "Diagnose build-stage failures, project health, cross-artifact integrity, and orphaned page assets.",
		MaxResult: 8000,
		Summarize: tools.SummarizeStripBodies,
		WhenToUse: `Use to inspect cross-artifact wiring, or to read back why an earlier BUILD
stage failed. All actions are read-only. These report on the project's stored
artifacts and its build history. They do not observe the RUNNING app's pages: a
browser-side failure (a blank page, a dead button) will not appear here. Reproduce
that with manage_testing, which drives the live server (HTTP, auth, page loads, JS
module execution) and is available during BUILD and LIVE. The one runtime signal
kept here is the message a function endpoint threw, in action=errors.`,
		Actions: []tools.Action{
			&DiagHealth{},
			&DiagErrors{},
			&DiagIntegrity{},
			&DiagUnlinkedArtifacts{},
		},
	})
}

// Read-only state actions

// DiagHealth returns a snapshot of the Go runtime, DB connection pool, and a
// per-project artifact count. Cheap; safe to call as a heartbeat.
type DiagHealth struct{}

func (a *DiagHealth) ActionName() string { return "health" }
func (a *DiagHealth) Doc() string        { return "Return process and DB health summary." }
func (a *DiagHealth) ReadOnly() bool     { return true }
func (a *DiagHealth) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	dbStats := ctx.DB.Stats()
	var pageCount int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0").Scan(&pageCount)
	return tools.Ok(map[string]any{
		"runtime": map[string]any{
			"go_version":    runtime.Version(),
			"num_goroutine": runtime.NumGoroutine(),
			"num_cpu":       runtime.NumCPU(),
			"os":            runtime.GOOS,
			"arch":          runtime.GOARCH,
		},
		"memory": map[string]any{
			"alloc_mb":       m.Alloc / 1024 / 1024,
			"total_alloc_mb": m.TotalAlloc / 1024 / 1024,
			"sys_mb":         m.Sys / 1024 / 1024,
			"num_gc":         m.NumGC,
		},
		"database": map[string]any{
			"open_connections": dbStats.OpenConnections,
			"in_use":           dbStats.InUse,
			"idle":             dbStats.Idle,
		},
		"project_stats": map[string]any{"pages": pageCount},
		"timestamp":     time.Now(),
	}), nil
}

// DiagErrors returns recent failures from two places: the engine run log (the
// unified of_run_log journal, where a failed stage is a kind='stage' row whose
// payload JSON carries {"status":"failed","error":...}) and of_runtime_errors,
// the causes behind the generic 5xx a sandboxed endpoint answers with.
type DiagErrors struct {
	Limit int `arg:"limit" desc:"Maximum entries of EACH kind returned: build-stage failures and runtime errors." default:"20"`
}

func (a *DiagErrors) ActionName() string { return "errors" }
func (a *DiagErrors) Doc() string {
	return "Recent failures: BUILD-STAGE failures from the engine run log (which stage failed, and why), plus the RUNTIME errors sandboxed function endpoints raised while serving a request. A function endpoint answers a caller with a generic 'function error' body, so this is where the thrown message is - read it before rewriting a handler that returns 500. A runtime error a PAGE throws in the browser is not here: reproduce that with manage_testing test_page."
}
func (a *DiagErrors) ReadOnly() bool { return true }
func (a *DiagErrors) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := ctx.DB.Query(
		`SELECT id, ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log
		 WHERE kind = 'stage' AND payload LIKE '%"status":"failed"%'
		 ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying errors: %w", err)
	}
	defer rows.Close()
	var errors []map[string]any
	for rows.Next() {
		var id, inTok, outTok, durationMs int
		var ref, payload, createdAt string
		if err := rows.Scan(&id, &ref, &payload, &inTok, &outTok, &durationMs, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning error: %w", err)
		}
		entry := map[string]any{
			"id": id, "event_type": ref + " failed",
			"tokens_used": inTok + outTok, "duration_ms": durationMs, "created_at": createdAt,
			"details": payload,
		}
		var p struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &p) == nil && p.Error != "" {
			entry["summary"] = p.Error
		}
		errors = append(errors, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying errors: %w", err)
	}
	// Runtime errors are the causes the HTTP responses could not carry, so they
	// belong in the same answer as the build failures rather than behind a
	// separate action nothing would think to call while a 500 is unexplained.
	// The build failures above are the primary answer, so a project DB whose
	// schema predates this table still gets them.
	runtimeErrs, rerr := db.RecentRuntimeErrors(ctx.DB, limit)
	if rerr != nil {
		return tools.Ok(errors), nil
	}
	for _, re := range runtimeErrs {
		errors = append(errors, map[string]any{
			"event_type": re.Source + " endpoint errored",
			"path":       re.Path,
			"summary":    re.Message,
			"created_at": re.CreatedAt,
		})
	}
	return tools.Ok(errors), nil
}

// DiagIntegrity does a lightweight cross-artifact lint pass: pages that
// reference a non-existent layout, pages that inline nav/footer (should live
// in the layout), forms that POST to a mismatching table.
type DiagIntegrity struct{}

func (a *DiagIntegrity) ActionName() string { return "integrity" }
func (a *DiagIntegrity) Doc() string {
	return "Cross-artifact lint: pages pointing at a missing layout, <nav>/<footer> markup that belongs in the layout, and forms whose submit target does not exist."
}
func (a *DiagIntegrity) ReadOnly() bool { return true }
func (a *DiagIntegrity) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var issues []string

	var layoutCount int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'layout' AND is_deleted = 0").Scan(&layoutCount)
	var pagesNeedingLayout int
	_ = ctx.DB.QueryRow(
		`SELECT COUNT(*) FROM of_files
		 WHERE kind = 'page' AND is_deleted = 0
		   AND COALESCE(json_extract(metadata, '$.layout'), '') != 'none'
		   AND COALESCE(json_extract(metadata, '$.layout'), '') != ''`,
	).Scan(&pagesNeedingLayout)
	if layoutCount == 0 && pagesNeedingLayout > 0 {
		issues = append(issues, "Pages reference a layout but no layouts exist. Create one with manage_files(action=\"save\", kind=\"layout\", file_path=\"default\", body=\"<full HTML doc with {{content}}>\").")
	}

	rows, err := ctx.DB.Query(
		`SELECT DISTINCT json_extract(metadata, '$.layout') FROM of_files
		 WHERE kind = 'page' AND is_deleted = 0
		   AND json_extract(metadata, '$.layout') IS NOT NULL
		   AND json_extract(metadata, '$.layout') != ''
		   AND json_extract(metadata, '$.layout') != 'none'
		   AND json_extract(metadata, '$.layout') NOT IN (SELECT path FROM of_files WHERE kind = 'layout' AND is_deleted = 0)`,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				issues = append(issues, fmt.Sprintf("Pages reference non-existent layout %q.", name))
			}
		}
		_ = rows.Err() // advisory lint pass; mirrors the silent query-error path
	}

	pageRows, err := ctx.DB.Query(
		"SELECT path, body FROM of_files WHERE kind = 'page' AND is_deleted = 0 AND status = 'published' LIMIT 500",
	)
	if err == nil {
		type pageRec struct{ path, content string }
		var pages []pageRec
		for pageRows.Next() {
			var p pageRec
			if pageRows.Scan(&p.path, &p.content) == nil {
				pages = append(pages, p)
			}
		}
		_ = pageRows.Err() // advisory lint pass; mirrors the silent query-error path
		pageRows.Close()
		for _, p := range pages {
			lower := strings.ToLower(p.content)
			if strings.Contains(lower, "<nav") {
				issues = append(issues, fmt.Sprintf("Page %q contains <nav> (should be in layout).", p.path))
			}
			if strings.Contains(lower, "<footer") {
				issues = append(issues, fmt.Sprintf("Page %q contains <footer> (should be in layout).", p.path))
			}
			issues = append(issues, formSinkMismatches(ctx, p.path, p.content)...)
		}
	}

	var cssCount, jsCount int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'file' AND path LIKE '%.css' AND is_deleted = 0").Scan(&cssCount)
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'file' AND path LIKE '%.js' AND is_deleted = 0").Scan(&jsCount)

	status := "healthy"
	if len(issues) > 0 {
		status = "issues_found"
	}
	return tools.Ok(map[string]any{
		"status": status,
		"issues": issues,
		"counts": map[string]any{
			"layouts": layoutCount, "css_assets": cssCount, "js_assets": jsCount,
		},
	}), nil
}

// DiagUnlinkedArtifacts lists page-scope JS/CSS that no page references. The
// renderer skips orphans, so this is a deterministically broken state the
// build loop can self-heal with manage_files attach.
type DiagUnlinkedArtifacts struct{}

func (a *DiagUnlinkedArtifacts) ActionName() string { return "unlinked_artifacts" }
func (a *DiagUnlinkedArtifacts) Doc() string {
	return "List page-scope JS/CSS files that no page loads (neither attached nor imported by a linked entry)."
}
func (a *DiagUnlinkedArtifacts) ReadOnly() bool { return true }
func (a *DiagUnlinkedArtifacts) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	paths := store.UnlinkedPageArtifacts(ctx.DB)
	status := "healthy"
	if len(paths) > 0 {
		status = "issues_found"
	}
	return tools.Ok(map[string]any{
		"status":             status,
		"unlinked_artifacts": paths,
		"count":              len(paths),
	}), nil
}
