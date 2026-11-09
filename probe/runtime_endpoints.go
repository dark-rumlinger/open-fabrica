package probe

// Connect/shape smokes for the non-WebSocket realtime surface: SSE streams, RSS/Atom
// feeds, and Web Push subscribe. Like the WebSocket smoke, each dials every endpoint
// of its kind against the live project server and turns "mounts but is misconfigured"
// (wrong content-type, a 500, a stream that never opens) into a fixable error issue.
// The headless page probe never touches these routes, so without this a feed whose
// column binding is wrong, or an SSE stream served as the wrong type, ships green.
//
// Each degrades to no issue on a transport error (a flaky dial must never block a
// build) and skips an auth-gated endpoint it has no usable token for, exactly like
// RunWebSocketSmoke. They route like the WS probe: dial baseURL (127.0.0.1:port) and
// set the project vhost as the Host header, which is how the server resolves the
// project.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const endpointProbeTimeout = 5 * time.Second

// endpointProbeClient returns after response headers (so an open SSE stream does not
// block it) and does not follow the project server anywhere surprising.
var endpointProbeClient = &http.Client{}

// RunSSESmoke dials every SSE endpoint's /api/<path>/stream route and asserts it
// answers 200 with a text/event-stream content type. An SSE endpoint that 404s,
// 500s, or is served as the wrong type leaves EventSource silently dead.
func RunSSESmoke(ctx context.Context, db *sql.DB, baseURL, hostHeader string) []ValidationIssue {
	if baseURL == "" || db == nil {
		return nil
	}
	token := authTokenFromContext(ctx)
	type streamEP struct {
		path         string
		requiresAuth bool
	}
	var eps []streamEP
	rows, err := db.Query("SELECT path, COALESCE(requires_auth,0) FROM of_stream_endpoints")
	if err != nil {
		return nil
	}
	for rows.Next() {
		var ep streamEP
		if rows.Scan(&ep.path, &ep.requiresAuth) == nil && ep.path != "" {
			eps = append(eps, ep)
		}
	}
	rows.Close()
	if rows.Err() != nil || len(eps) == 0 {
		return nil
	}

	var issues []ValidationIssue
	for _, ep := range eps {
		if ctx.Err() != nil {
			break
		}
		route := "/api/" + ep.path + "/stream"
		q := ""
		if ep.requiresAuth {
			if token == "" {
				continue // no usable token: skip rather than report a false 401
			}
			q = "?token=" + token
		}
		reqCtx, cancel := context.WithTimeout(ctx, endpointProbeTimeout)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+route+q, nil)
		if hostHeader != "" {
			req.Host = hostHeader
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, derr := endpointProbeClient.Do(req)
		if derr != nil {
			cancel()
			continue // transport: degrade
		}
		status := resp.StatusCode
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()
		cancel()
		if status != http.StatusOK {
			issues = append(issues, endpointIssue(route, "sse_status",
				fmt.Sprintf("SSE endpoint %q answered %d, not 200 - EventSource('%s') will error on open. Check the stream endpoint's table/auth wiring.", route, status, route)))
		} else if !strings.Contains(strings.ToLower(ct), "text/event-stream") {
			issues = append(issues, endpointIssue(route, "sse_contenttype",
				fmt.Sprintf("SSE endpoint %q is served as %q, not text/event-stream - the browser will not treat it as an event stream. Serve it through the stream endpoint, never a plain JSON handler.", route, ct)))
		}
	}
	return issues
}

// RunFeedSmoke dials every feed endpoint's /feed/<path> route and asserts it renders
// a 200 XML feed. A wrong column binding or filter makes the feed 500 or render
// empty, which only a reader would otherwise notice.
func RunFeedSmoke(ctx context.Context, db *sql.DB, baseURL, hostHeader string) []ValidationIssue {
	if baseURL == "" || db == nil {
		return nil
	}
	var paths []string
	rows, err := db.Query("SELECT path FROM of_feed_endpoints")
	if err != nil {
		return nil
	}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			paths = append(paths, p)
		}
	}
	rows.Close()
	if rows.Err() != nil || len(paths) == 0 {
		return nil
	}

	var issues []ValidationIssue
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		route := "/feed/" + p
		reqCtx, cancel := context.WithTimeout(ctx, endpointProbeTimeout)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+route, nil)
		if hostHeader != "" {
			req.Host = hostHeader
		}
		resp, derr := endpointProbeClient.Do(req)
		if derr != nil {
			cancel()
			continue
		}
		status := resp.StatusCode
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		head, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		cancel()
		if status != http.StatusOK {
			issues = append(issues, endpointIssue(route, "feed_status",
				fmt.Sprintf("feed %q answered %d, not 200 - check the feed's table binding and where filter (a bad column makes the render fail).", route, status)))
			continue
		}
		looksXML := strings.Contains(ct, "xml") || strings.Contains(string(head), "<?xml") || strings.Contains(string(head), "<rss") || strings.Contains(string(head), "<feed")
		if !looksXML {
			issues = append(issues, endpointIssue(route, "feed_shape",
				fmt.Sprintf("feed %q answered 200 but not as XML (content-type %q) - it is not rendering an RSS/Atom document.", route, ct)))
		}
	}
	return issues
}

// RunPushSmoke posts a synthetic PushSubscription to every /api/<path>/push/subscribe
// route and asserts it is accepted, then deletes the probe subscription so the smoke
// leaves no residue. It proves the subscribe route is wired before a real browser
// ever opts in.
func RunPushSmoke(ctx context.Context, db *sql.DB, baseURL, hostHeader string) []ValidationIssue {
	if baseURL == "" || db == nil {
		return nil
	}
	token := authTokenFromContext(ctx)
	type pushEP struct {
		path         string
		requiresAuth bool
	}
	var eps []pushEP
	rows, err := db.Query("SELECT path, COALESCE(requires_auth,0) FROM of_push_endpoints")
	if err != nil {
		return nil
	}
	for rows.Next() {
		var ep pushEP
		if rows.Scan(&ep.path, &ep.requiresAuth) == nil && ep.path != "" {
			eps = append(eps, ep)
		}
	}
	rows.Close()
	if rows.Err() != nil || len(eps) == 0 {
		return nil
	}

	var issues []ValidationIssue
	for _, ep := range eps {
		if ctx.Err() != nil {
			break
		}
		if ep.requiresAuth && token == "" {
			continue // subscribe needs a token the probe does not have: skip
		}
		route := "/api/" + ep.path + "/push/subscribe"
		// A synthetic subscription; the handler only checks the three fields are non-empty.
		endpointURL := "https://probe.invalid/of-probe-" + ep.path
		body := strings.NewReader(fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":"of-probe-p256dh","auth":"of-probe-auth"}}`, endpointURL))
		reqCtx, cancel := context.WithTimeout(ctx, endpointProbeTimeout)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+route, body)
		if hostHeader != "" {
			req.Host = hostHeader
		}
		req.Header.Set("Content-Type", "application/json")
		if ep.requiresAuth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, derr := endpointProbeClient.Do(req)
		if derr != nil {
			cancel()
			continue
		}
		status := resp.StatusCode
		resp.Body.Close()
		cancel()
		if status < 200 || status >= 300 {
			issues = append(issues, endpointIssue(route, "push_subscribe",
				fmt.Sprintf("push subscribe %q answered %d, not 2xx - browsers cannot register a PushSubscription, so no notification can ever be delivered.", route, status)))
			continue
		}
		// Clean up the probe subscription so it is never delivered to for real.
		_, _ = db.Exec("DELETE FROM of_push_subscriptions WHERE endpoint_url = ?", endpointURL)
	}
	return issues
}

// endpointIssue builds a deterministic error issue for a non-WS realtime endpoint,
// with a per-mode Signature so distinct failures on one route stay distinct.
func endpointIssue(route, signature, message string) ValidationIssue {
	return ValidationIssue{
		Severity:      "error",
		Category:      "endpoint",
		Page:          route,
		Deterministic: true,
		Signature:     signature,
		Message:       message,
	}
}
