package validators

// The realtime client-API-vs-model validator. Each realtime model answers a different
// slice of the 'ofnet' helper: net.action/net.onState need an authoritative reducer,
// net.onMatched needs a matchmaking lobby, and a room_gated endpoint needs connect()'s
// ?join= handling. Pointed at the wrong model each one fails SILENTLY in the browser:
// an action is relayed as a plain frame and changes no state, onState/onMatched never
// fire, and a raw socket to a gated room is refused 401 before it upgrades - every
// probe green, the feature dead.
//
// Static and conservative: a file is judged only when it connects to exactly ONE
// literal endpoint path (connect("game", ...)), so a method call is attributed to that
// endpoint and nothing else. A path built from variables is invisible (the acceptable
// false-negative), as in raw_ws_auth.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"openfabrica/internal/wsendpoint"
)

type realtimeAPIValidator struct{}

func (realtimeAPIValidator) Name() string { return "realtime_api_mismatch" }

var (
	connectLiteralRe = regexp.MustCompile(`\bconnect\(\s*["']([A-Za-z0-9_\-/]+)["']`)
	actionCallRe     = regexp.MustCompile(`\.action\(\s*["'\x60]`)
	onStateCallRe    = regexp.MustCompile(`\.onState\(`)
	onMatchedCallRe  = regexp.MustCompile(`\.onMatched\(`)
)

func (realtimeAPIValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	rows, err := wsendpoint.LoadAll(pdb)
	if err != nil || len(rows) == 0 {
		return nil
	}
	byPath := make(map[string]*wsendpoint.Row, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
	}

	var issues []Issue
	add := func(file, sig, msg string) {
		issues = append(issues, Issue{
			Severity: "error", Category: "realtime_api_mismatch", File: file,
			Deterministic: true, Signature: sig, Message: msg,
		})
	}
	for _, f := range jsAndPageFiles(pdb) {
		// Raw sockets to a room_gated endpoint: the first peer never learns its
		// _joinToken handling and every later peer is refused 401 before the upgrade.
		// A raw socket that builds ?join= into its URL carries the token, so it is fine.
		if rawSocketRe.MatchString(f.body) && !strings.Contains(f.body, "join=") {
			for _, r := range rows {
				if r.RoomGated && strings.Contains(f.body, "/api/"+r.Path+"/ws") {
					add(f.path, "raw:"+r.Path, fmt.Sprintf("`%s` opens a raw `new WebSocket()` to `/api/%s/ws`, which is room_gated: entry needs the per-room ?join= token, which a hand-built URL does not carry, so every peer after the first is refused HTTP 401 before the upgrade. Connect with `import { connect } from 'ofnet'; connect(%q, { room, join: token })` - the helper forwards ?join= and exposes the socket as net.ws.", f.path, r.Path, r.Path))
				}
			}
		}

		paths := map[string]bool{}
		for _, m := range connectLiteralRe.FindAllStringSubmatch(f.body, -1) {
			paths[strings.Trim(m[1], "/")] = true
		}
		if len(paths) != 1 {
			continue // none, or several: a method call cannot be attributed to one endpoint
		}
		var path string
		for p := range paths {
			path = p
		}
		r := byPath[path]
		if r == nil {
			continue // not a realtime endpoint (or not built yet): other gates own that
		}
		model := r.EffectiveModel()
		if model != wsendpoint.ModelAuthoritative {
			var used []string
			if actionCallRe.MatchString(f.body) {
				used = append(used, "net.action()")
			}
			if onStateCallRe.MatchString(f.body) {
				used = append(used, "net.onState()")
			}
			if len(used) > 0 {
				add(f.path, "auth:"+path, fmt.Sprintf("`%s` calls %s on `%s`, a %s endpoint - those need an authoritative reducer (manage_authoritative create + set_reducer). Here an action is relayed as a plain frame that changes no state, and onState never fires. Either build `%s` with manage_authoritative, or use this model's lanes (net.send for continuous state, net.event for discrete messages, net.on(type) to receive).", f.path, strings.Join(used, " and "), path, model, path))
			}
		}
		if model != wsendpoint.ModelMatchmaking && onMatchedCallRe.MatchString(f.body) {
			add(f.path, "match:"+path, fmt.Sprintf("`%s` waits on net.onMatched() for `%s`, a %s endpoint - only a manage_matchmaking lobby sends the `matched` frame, so the callback never fires and players wait forever. Pair on a manage_matchmaking endpoint, then connect the matched peers to `%s` with the assigned room id.", f.path, path, model, path))
		}
	}
	return issues
}

func init() { RegisterValidator(realtimeAPIValidator{}) }
