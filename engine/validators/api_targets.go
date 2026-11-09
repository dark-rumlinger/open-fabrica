package validators

// The static API-target validator: catches a page or client module calling an
// /api/ path no endpoint serves. Nothing else covers this - the internal-link
// validator skips /api/ by contract, and the runtime probe records only the
// main-frame document status, so a page whose every fetch misses still probes
// green. Deliberately narrow, because a false positive bounces a correct build:
// static string literals only, path existence only (never method), and prefix
// matching against known base paths (so a real endpoint is never called dead).
// Severity is a speculative warning: the structural signal cannot prove intent.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type apiTargetValidator struct{}

func (apiTargetValidator) Name() string { return "api_target" }

// apiFetchCallRe matches an apiFetch("...") call with a literal first argument.
// The path is relative to /api/ unless it starts with "/", in which case the
// helper uses it verbatim (tools/auth/auth_helper.go).
var apiFetchCallRe = regexp.MustCompile(`\bapiFetch\s*\(\s*['"]([^'"]+)['"]`)

// apiLiteralRe matches an "/api/..." string literal, the shape a bare fetch()
// uses. Anchored on the literal rather than the call so it also catches a URL
// built into a const and passed to fetch.
var apiLiteralRe = regexp.MustCompile(`['"](/api/[^'"?#]*)['"]`)

// servedRoutes splits what the API serves into paths that answer the BARE
// /api/<path> (CRUD/function/query, and their /<id> sub-routes) and the exact
// suffixed routes the non-CRUD kinds answer (/api/<path>/upload, /stream, /run,
// login, ...). The split matters: an upload endpoint at path "p" serves
// /api/p/upload, NOT /api/p, so a page POSTing the bare /api/p is dead even
// though the base path exists.
type servedRoutes struct {
	bare map[string]bool // serves /api/<path> (prefix-matched, so /<id> counts)
	full map[string]bool // serves exactly /api/<path>/<sub_segment>
}

// servableRoutes reads the of_endpoints spine (split by sub_segment) plus the
// per-kind CRUD table. CRUD is read directly too: it dispatches without needing
// a spine row (server/public/dynamic_api.go), so the spine alone would
// under-report on any project whose CRUD rows predate their claim.
func servableRoutes(pdb *sql.DB) (servedRoutes, bool) {
	sr := servedRoutes{bare: map[string]bool{}, full: map[string]bool{}}
	readOK := true
	if rows, err := pdb.Query(`SELECT DISTINCT path, sub_segment FROM of_endpoints`); err != nil {
		readOK = false
	} else {
		for rows.Next() {
			var p, sub string
			if rows.Scan(&p, &sub) == nil {
				p = strings.Trim(strings.TrimSpace(p), "/")
				sub = strings.Trim(strings.TrimSpace(sub), "/")
				if p == "" {
					continue
				}
				if sub == "" {
					sr.bare[p] = true // CRUD/function/query answer the bare path
				} else {
					sr.full[p+"/"+sub] = true // suffixed-only route
				}
			}
		}
		_ = rows.Err()
		_ = rows.Close()
	}
	if rows, err := pdb.Query(`SELECT DISTINCT path FROM of_api_endpoints`); err != nil {
		readOK = false
	} else {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				if p = strings.Trim(strings.TrimSpace(p), "/"); p != "" {
					sr.bare[p] = true
				}
			}
		}
		_ = rows.Err()
		_ = rows.Close()
	}
	return sr, readOK
}

// apiTargetPath normalizes one extracted literal to a path under /api/, or
// returns ok=false when the literal does not address the project's API.
func apiTargetPath(literal string, fromAPIFetch bool) (string, bool) {
	s := strings.TrimSpace(literal)
	if s == "" {
		return "", false
	}
	// A dynamic segment cannot be resolved statically.
	if strings.ContainsAny(s, "${+`") {
		return "", false
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return "", false // an external URL is not this project's API
	}
	if fromAPIFetch && !strings.HasPrefix(s, "/") {
		s = "/api/" + s
	}
	if !strings.HasPrefix(s, "/api/") {
		return "", false
	}
	s = strings.TrimPrefix(s, "/api/")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return "", false
	}
	return s, true
}

// serves reports whether target is served: a bare base is a segment-aligned
// prefix of it (so /api/<crud>/<id> counts), or a suffixed route matches a
// segment-aligned prefix (so /api/<path>/upload counts while the bare
// /api/<path> does not).
func (sr servedRoutes) serves(target string) bool {
	segs := strings.Split(target, "/")
	for i := len(segs); i > 0; i-- {
		prefix := strings.Join(segs[:i], "/")
		if sr.bare[prefix] || sr.full[prefix] {
			return true
		}
	}
	return false
}

func (apiTargetValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	routes, readOK := servableRoutes(pdb)
	if !readOK {
		return nil // could not read the endpoint tables: self-skip rather than guess
	}
	// An EMPTY endpoint set is still checked: pages calling /api/... in a project that
	// created no endpoint at all is the worst form of this miss, not noise.

	// file -> the dead targets it names, deduped: one finding per file lets a
	// single fix-pass correct them all.
	dead := map[string]map[string]bool{}
	for _, f := range jsAndPageFiles(pdb) {
		path, body := f.path, f.body
		add := func(literal string, fromAPIFetch bool) {
			target, ok := apiTargetPath(literal, fromAPIFetch)
			if !ok || routes.serves(target) {
				return
			}
			if dead[path] == nil {
				dead[path] = map[string]bool{}
			}
			dead[path]["/api/"+target] = true
		}
		for _, m := range apiFetchCallRe.FindAllStringSubmatch(body, -1) {
			add(m[1], true)
		}
		for _, m := range apiLiteralRe.FindAllStringSubmatch(body, -1) {
			add(m[1], false)
		}
	}

	known := make([]string, 0, len(routes.bare)+len(routes.full))
	for b := range routes.bare {
		known = append(known, "/api/"+b)
	}
	for f := range routes.full {
		known = append(known, "/api/"+f)
	}
	sort.Strings(known)
	knownText := strings.Join(known, ", ")
	if len(known) == 0 {
		knownText = "none - this project has created no API endpoint yet"
	}

	files := make([]string, 0, len(dead))
	for f := range dead {
		files = append(files, f)
	}
	sort.Strings(files)

	issues := make([]Issue, 0, len(files))
	for _, f := range files {
		targets := make([]string, 0, len(dead[f]))
		for t := range dead[f] {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		issues = append(issues, Issue{
			Severity:    "warning",
			Category:    "api_target",
			File:        f,
			Speculative: true,
			Message: fmt.Sprintf("`%s` calls %s, but no endpoint is registered at that path - the request 404s and this feature does nothing. "+
				"Nothing else catches it: the page still loads, so the runtime probe stays green. Registered API paths: %s. "+
				"Either point the call at one of those, or create the endpoint (`manage_api action=create` for table-backed CRUD, "+
				"`manage_function action=create` for computed request logic). If the path is built at runtime and only looks literal here, no change is needed.",
				f, strings.Join(targets, ", "), knownText),
		})
	}
	return issues
}

func init() {
	RegisterValidator(apiTargetValidator{})
}
