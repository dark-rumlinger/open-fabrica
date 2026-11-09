package validators

// The static raw-WebSocket-vs-auth validator: catches client code opening a raw
// `new WebSocket()` against a requires_auth endpoint. A browser socket cannot
// send an Authorization header, so a raw socket that does not carry the token
// itself (?token= on its URL) cannot authenticate - the server refuses the
// upgrade with 401 and the feature ships dead; the 'ofnet' connect() helper
// carries it for you. Static: fires only when a file both constructs a raw
// socket AND names an auth-gated endpoint's literal route, and never passes
// token= - a URL built from variables is invisible (the acceptable
// false-negative). Platform-managed files (js/ofnet.js) are excluded.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type rawWSAuthValidator struct{}

func (rawWSAuthValidator) Name() string { return "raw_ws_auth" }

// rawSocketRe matches a raw browser socket construction. The ofnet helper's internal
// use never appears here because platform-managed files are skipped.
var rawSocketRe = regexp.MustCompile(`new\s+WebSocket\s*\(`)

func (rawWSAuthValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	// Auth-gated WS endpoints. A missing of_ws_endpoints table (no WS in this
	// project) or read error reports nothing - the other gates own that.
	rows, err := pdb.Query(`SELECT path FROM of_ws_endpoints WHERE COALESCE(requires_auth, 0) = 1`)
	if err != nil {
		return nil
	}
	var gated []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			gated = append(gated, p)
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil || len(gated) == 0 {
		return nil
	}

	var issues []Issue
	for _, f := range jsAndPageFiles(pdb) {
		path, body := f.path, f.body
		if !rawSocketRe.MatchString(body) {
			continue
		}
		// A raw socket that carries the token itself (?token= on its URL) authenticates;
		// the server accepts that form, so only a socket without one is dead.
		if strings.Contains(body, "token=") {
			continue
		}
		for _, ep := range gated {
			if !strings.Contains(body, "/api/"+ep+"/ws") {
				continue
			}
			issues = append(issues, Issue{
				Severity:      "error",
				Category:      "raw_ws_auth",
				File:          path,
				Deterministic: true,
				Message: fmt.Sprintf("`%s` opens a raw `new WebSocket()` to `/api/%s/ws`, but that endpoint has requires_auth=true. A browser socket cannot send an "+
					"Authorization header and this one passes no ?token= on its URL, so the server refuses the upgrade with HTTP 401 and this feature is dead for every "+
					"signed-in user. Connect through the platform helper instead: `import { connect } from 'ofnet'; const net = connect(%q, { room: roomId })` - it auto-carries the logged-in "+
					"user's token as ?token= (plus ?join= for room_gated endpoints) and exposes the underlying socket as net.ws if you need the raw handle.",
					path, ep, ep),
			})
		}
	}
	return issues
}

// platformManaged reports whether an of_files metadata blob marks the file as
// platform-owned (js/ofnet.js, js/e2ee.js) - those legitimately construct raw
// sockets and are not model-editable anyway.
func platformManaged(meta string) bool {
	if meta == "" {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(meta), &m) != nil {
		return false
	}
	v, _ := m["platform_managed"].(bool)
	return v
}

func init() {
	RegisterValidator(rawWSAuthValidator{})
}
