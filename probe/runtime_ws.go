package probe

// WebSocket smoke pass for the whole-app probe. The headless page probe never touches
// a WebSocket endpoint - a plain GET answers 426, which test_url treats as healthy -
// so a relay that upgrades and then never emits its welcome frame ships green. This
// pass dials every of_ws_endpoints route, reads the first frame, and asserts the
// welcome contract, turning that failure into a fixable error issue.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"

	"openfabrica/internal/roomactor"
)

const wsSmokeReadTimeout = 5 * time.Second

type wsEndpoint struct {
	path          string
	roomGated     bool
	requiresAuth  bool
	tenantColumn  string
	e2ee          bool
	authoritative bool
	hasView       bool // the reducer defined view(): peer-hiding is legitimate
	tickHz        int
	matchmaking   bool
	groupSize     int
}

// RunWebSocketSmoke dials each WebSocket endpoint, completes the handshake, and
// verifies the relay emits a welcome frame. Returns one error issue per broken
// endpoint. No-op when there is no base URL or no endpoint. Degrades to no
// issues on environmental problems so a flaky dial never blocks the build on its
// own - a genuinely broken relay fails deterministically every round.
func RunWebSocketSmoke(ctx context.Context, db *sql.DB, baseURL, hostHeader string) []ValidationIssue {
	if baseURL == "" || db == nil {
		return nil
	}
	endpoints := loadWSEndpoints(db)
	if len(endpoints) == 0 {
		return nil
	}
	wsBase := strings.Replace(baseURL, "http://", "ws://", 1)
	wsBase = strings.Replace(wsBase, "https://", "wss://", 1)

	// The build's probe auth token rides the context (mintProbeAuthToken +
	// WithAuthToken in runValidators). Auth-gated endpoints are dialed with it on
	// ?token= - the browser-equivalent path the 'ofnet' connect() helper sends - so a
	// healthy requires_auth relay is verified rather than reported as a false 401.
	token := authTokenFromContext(ctx)

	var issues []ValidationIssue
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			break
		}
		if iss := wsSmokeOne(ctx, wsBase, hostHeader, ep, token); iss != nil {
			// Welcome broke: report that and don't probe delivery on a dead socket.
			issues = append(issues, *iss)
			continue
		}
		// Welcome passed. Skip the delivery flow exactly where the welcome smoke
		// skips (an auth endpoint with no usable token, or a tenant-scoped one the
		// probe can't drive a second peer into) rather than emit a false failure.
		if ep.requiresAuth && (token == "" || ep.tenantColumn != "") {
			continue
		}
		issues = append(issues, wsDeliveryIssues(ctx, wsBase, hostHeader, ep, token)...)
	}
	return issues
}

// wsSmokeOne dials one endpoint as an honest first peer (no join token) and
// requires a welcome frame. A room_gated endpoint must additionally hand the
// first peer a _joinToken, without it no invite link can be built.
func wsSmokeOne(ctx context.Context, wsBase, hostHeader string, ep wsEndpoint, token string) *ValidationIssue {
	route := "/api/" + ep.path + "/ws"
	// Auth-gated endpoints need the token on ?token= (a browser socket can't send
	// an Authorization header). Without a usable token the probe cannot
	// authenticate, so it would only 401 - skip rather than emit a false failure.
	// A tenant-scoped endpoint needs a tenant-scoped token this probe can't mint.
	authQ := ""
	if ep.requiresAuth {
		if token == "" || ep.tenantColumn != "" {
			return nil
		}
		authQ = "&token=" + token
	}
	url := fmt.Sprintf("%s%s?room=of-probe-%s%s", wsBase, route, ep.path, authQ)

	dialCtx, cancelDial := context.WithTimeout(ctx, wsSmokeReadTimeout)
	defer cancelDial()
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{Host: hostHeader})
	if err != nil {
		status := "no HTTP response"
		if resp != nil {
			status = resp.Status
		}
		return &ValidationIssue{
			Severity:      "error",
			Category:      "websocket",
			Page:          route,
			Deterministic: true,
			// Signature names the failure MODE, so this smoke finding stays distinct
			// from the authoritative-reducer smoke on the same endpoint (both are
			// Category "websocket", Page "/api/<path>/ws", File ""), and so a cleared
			// handshake failure that reveals a missing-welcome failure earns its own
			// fix-pass instead of demoting as a survivor. See tools/store/issue.go.
			Signature: "handshake",
			Message: fmt.Sprintf(
				"WebSocket endpoint %q failed its handshake (%s): %v. The route is mounted but a real client cannot connect - verify the endpoint with manage_testing action=test_websocket endpoint_path=%s (add two_peer=true if it is room_gated).",
				route, status, err, ep.path,
			),
		}
	}
	defer conn.Close(websocket.StatusNormalClosure, "probe done")

	readCtx, cancelRead := context.WithTimeout(ctx, wsSmokeReadTimeout)
	defer cancelRead()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return &ValidationIssue{
			Severity:      "error",
			Category:      "websocket",
			Page:          route,
			Deterministic: true,
			Signature:     "welcome",
			Message: fmt.Sprintf(
				"WebSocket endpoint %q upgraded but never emitted a welcome frame (%v) - the relay is not speaking the join/welcome contract, so clients hang on connect. Verify with manage_testing action=test_websocket endpoint_path=%s.",
				route, err, ep.path,
			),
		}
	}
	var frame map[string]any
	if json.Unmarshal(data, &frame) != nil || frame["_type"] != "welcome" {
		return &ValidationIssue{
			Severity:      "error",
			Category:      "websocket",
			Page:          route,
			Deterministic: true,
			Signature:     "first_frame",
			Message: fmt.Sprintf(
				"WebSocket endpoint %q sent a first frame that is not a welcome frame - clients cannot bootstrap presence. Verify with manage_testing action=test_websocket endpoint_path=%s.",
				route, ep.path,
			),
		}
	}
	if ep.roomGated {
		if tok, _ := frame["_joinToken"].(string); tok == "" {
			return &ValidationIssue{
				Severity:      "error",
				Category:      "websocket",
				Page:          route,
				Deterministic: true,
				Signature:     "join_token",
				Message: fmt.Sprintf(
					"WebSocket endpoint %q is room_gated but the first peer's welcome carried no _joinToken, so the shareable invite link cannot be built and a second peer can never join. Verify with manage_testing action=test_websocket endpoint_path=%s two_peer=true.",
					route, ep.path,
				),
			}
		}
	}
	return nil
}

// loadWSEndpoints lists the project's WebSocket endpoints and their gating flags.
func loadWSEndpoints(db *sql.DB) []wsEndpoint {
	rows, err := db.Query("SELECT path, COALESCE(room_gated,0), COALESCE(requires_auth,0), COALESCE(tenant_column,''), COALESCE(e2ee,0), COALESCE(reducer,''), COALESCE(tick_hz,0), COALESCE(matchmaking,0), COALESCE(group_size,2) FROM of_ws_endpoints")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []wsEndpoint
	for rows.Next() {
		var ep wsEndpoint
		var reducer string
		if rows.Scan(&ep.path, &ep.roomGated, &ep.requiresAuth, &ep.tenantColumn, &ep.e2ee, &reducer, &ep.tickHz, &ep.matchmaking, &ep.groupSize) == nil && ep.path != "" {
			ep.authoritative = strings.TrimSpace(reducer) != ""
			// hasView decides whether a peer missing from another peer's state is a bug
			// or a legitimate hidden-info projection. Default true (no gate) when the
			// reducer will not compile here, so an unanalyzable endpoint is never failed.
			ep.hasView = true
			if ep.authoritative {
				if red, cerr := roomactor.Compile(reducer); cerr == nil {
					ep.hasView = red.HasView()
				}
			}
			out = append(out, ep)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}
