package validators

// The static auth-page-shell validator: catches a login screen rendered inside
// the authenticated app shell. The `default` layout is the signed-in chrome, so
// a `/login` wrapped in it shows the nav to an unauthenticated visitor - and
// nothing else flags it (the page serves 200, modules load, links resolve). The
// fix: metadata.layout:"none" renders standalone. Static (of_files), resolving
// each page's layout the way the renderer does (empty/missing -> "default").

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type authPageShellValidator struct{}

func (authPageShellValidator) Name() string { return "auth_page_shell" }

// passwordInputRe matches a password field, the load-bearing signal that a page
// is a credential screen. The runtime never needs JS for this - it is in the
// static HTML fragment.
var passwordInputRe = regexp.MustCompile(`(?i)type\s*=\s*["']password["']`)

// layoutChromeRe matches the app-shell markers a signed-in layout carries: a
// <nav>/<aside> landmark or a sidebar class/id. A layout with none of these is
// already a bare frame, so wrapping a login page in it is fine and not flagged.
var layoutChromeRe = regexp.MustCompile(`(?i)(<nav\b|<aside\b|role\s*=\s*["']navigation["']|sidebar)`)

func (authPageShellValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	layouts := layoutBodies(pdb)
	if layouts == nil {
		return nil // no layout table / read error: the other gates own that
	}

	rows, err := pdb.Query(
		`SELECT path, COALESCE(body,''), COALESCE(metadata,'')
		 FROM of_files WHERE kind = 'page' AND is_deleted = 0`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var flagged []string
	seen := map[string]bool{}
	for rows.Next() {
		var path, body, metadata string
		if rows.Scan(&path, &body, &metadata) != nil {
			continue
		}
		if !isAuthPage(path, body) {
			continue
		}
		layout := pageLayoutName(metadata)
		if layout == "none" {
			continue // already a standalone screen - correct
		}
		name := layout
		if name == "" {
			name = "default"
		}
		shell, ok := layouts[name]
		if !ok && name != "default" {
			shell, ok = layouts["default"]
		}
		if !ok || !layoutChromeRe.MatchString(shell) {
			continue // no chrome-bearing layout actually wraps this page
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		flagged = append(flagged, path)
	}
	if rows.Err() != nil {
		return nil // partial read: self-skip rather than guess
	}

	if len(flagged) == 0 {
		return nil
	}
	sort.Strings(flagged) // deterministic issue order for the fix-pass

	issues := make([]Issue, 0, len(flagged))
	for _, p := range flagged {
		issues = append(issues, Issue{
			Severity:      "error",
			Category:      "auth_page_in_shell",
			Page:          p,
			Deterministic: true,
			Message: "`" + p + "` is a login screen but renders inside the `default` layout - the authenticated app shell (sidebar, nav, signed-in " +
				"identity). The owner sees the dashboard chrome before signing in, and the nav flashes for an unauthenticated visitor. Save this page " +
				"with `metadata: {\"layout\":\"none\"}` so it renders as a standalone screen; keep `redirectIfLoggedIn(...)` (imported from 'auth') at the top of its script.",
		})
	}
	return issues
}

// pageLayoutName extracts the lowercased `layout` field from a page's metadata
// JSON, returning "" when the metadata is absent, not valid JSON, or has no
// layout key (the renderer treats all of those as the "default" layout).
func pageLayoutName(metadata string) string {
	var m struct {
		Layout string `json:"layout"`
	}
	if json.Unmarshal([]byte(metadata), &m) != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(m.Layout))
}

// isAuthPage reports whether a page is a sign-in screen (vs. an in-app
// change-password form, which legitimately lives inside the shell): it has a
// password field AND either a login-ish route or the login bounce
// `redirectIfLoggedIn` the auth contract puts at the top of every /login page.
// An in-app page guards with requireAuth/requireRole, never redirectIfLoggedIn.
func isAuthPage(path, body string) bool {
	if !passwordInputRe.MatchString(body) {
		return false
	}
	if strings.Contains(body, "redirectIfLoggedIn") {
		return true
	}
	lp := strings.ToLower(path)
	for _, s := range []string{"login", "signin", "sign-in", "/auth"} {
		if strings.Contains(lp, s) {
			return true
		}
	}
	return false
}

func init() {
	RegisterValidator(authPageShellValidator{})
}
