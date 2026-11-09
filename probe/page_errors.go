package probe

// The runtime-error listener shared by the page-load probe (probeRuntime) and the
// flow runner (RunFlowTests): one ListenTarget that records uncaught throws,
// console.error, and failed requests, classified identically for both so a JS error
// means the same thing whether it fired on load or after a scripted click.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"openfabrica/analysis"
	"openfabrica/internal/stringutil"
)

// pageErrorCollector accumulates tagged runtime errors ("[uncaught] ...",
// "[console.error] ...", "[load_failed] ...") for one tab, plus the main-frame
// response the navigation-failure report enriches itself with.
type pageErrorCollector struct {
	mu   sync.Mutex
	errs []string
	// reqURLs maps an in-flight request id to its URL: EventLoadingFailed carries only the
	// id, so recording the URL on send recovers the origin and resource name on failure.
	reqURLs map[network.RequestID]string
	// Main-frame HTTP response from the first document response, so the nav-failure
	// branch can report "status: 404" instead of the bare chromedp error.
	mainFrameStatus int64
	mainFrameURL    string
	// reqMethods and httpFails record same-origin page, fetch and XHR responses with
	// an error status, so a failed flow step can name the request that went wrong.
	reqMethods map[network.RequestID]string
	httpFails  []httpFail
	// apiCalls records the page's same-origin /api/ fetch and XHR responses of any
	// status, newest last and bounded, so a failed flow step can show what the server
	// actually returned: a 200 without the field the page reads looks like a healthy
	// request in every other report.
	apiCalls []httpFail
	// logs is a bounded tail of the page's console.log/info/warn/debug output, kept for
	// the failure report of a flow step: a silent failure (a promise that never settles)
	// shows nothing else, and the app's own log lines are the only way to see inside it.
	logs []string
}

// maxConsoleLogs bounds the console tail kept per tab, and maxConsoleLine one line of it.
// maxAPICalls bounds the API responses kept per tab.
const (
	maxConsoleLogs = 60
	maxConsoleLine = 240
	maxAPICalls    = 12
)

// httpFail is one same-origin request that got a 4xx or 5xx response.
type httpFail struct {
	id      network.RequestID
	method  string
	url     string
	status  int64
	session string // set by the flow runner when it drains a session's collector
	step    int    // index of the flow step it happened during, set by the flow runner
}

// listenPageErrors attaches the collector to tabCtx. pageURL names the host being
// probed: a failed request to any other host is cross-origin (a declared CDN/font the
// project can't fix), not the project's own asset. The runtime and network domains
// must be enabled on the tab for the events to fire.
func listenPageErrors(tabCtx context.Context, pageURL string) *pageErrorCollector {
	c := &pageErrorCollector{reqURLs: make(map[network.RequestID]string), reqMethods: make(map[network.RequestID]string)}
	probeHost := hostOf(pageURL)
	chromedp.ListenTarget(tabCtx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventExceptionThrown:
			if e.ExceptionDetails != nil {
				// Bypass add's 600-byte trim: the stack IS the diagnosis for an uncaught
				// throw, and the formatter appends the first project-code frame so a fix
				// starts in the right file, not the vendor bundle at the top of the trace.
				msg := analysis.FormatExceptionForModel(e.ExceptionDetails, 1200)
				if msg != "" {
					c.mu.Lock()
					c.errs = append(c.errs, "[uncaught] "+msg)
					c.mu.Unlock()
				}
			}
		case *runtime.EventConsoleAPICalled:
			parts := make([]string, 0, len(e.Args))
			for _, a := range e.Args {
				if a == nil {
					continue
				}
				if len(a.Value) > 0 {
					var str string
					if json.Unmarshal(a.Value, &str) == nil {
						parts = append(parts, str) // a logged string without its JSON quotes
					} else {
						parts = append(parts, string(a.Value))
					}
				} else if a.Description != "" {
					parts = append(parts, a.Description)
				}
			}
			if e.Type != "error" {
				c.addLog(string(e.Type), strings.Join(parts, " "))
				return
			}
			c.add("console.error", strings.Join(parts, " "))
		case *network.EventRequestWillBeSent:
			if e.Request != nil && e.Request.URL != "" {
				c.mu.Lock()
				c.reqURLs[e.RequestID] = e.Request.URL
				c.reqMethods[e.RequestID] = e.Request.Method
				c.mu.Unlock()
			}
		case *network.EventLoadingFailed:
			// Recover the failing URL (recorded on request-send) so we can both
			// name the resource in the report and decide same- vs cross-origin.
			c.mu.Lock()
			failedURL := c.reqURLs[e.RequestID]
			c.mu.Unlock()

			// Classify by origin then resource type (see loadFailureKind): cross-origin and
			// cosmetic same-origin types are soft, only a same-origin functional resource is hard.
			sameOrigin := failedURL == "" || probeHost == "" || hostOf(failedURL) == probeHost
			kind := loadFailureKind(e.Type, sameOrigin)
			// Name the resource by URL (empty BlockedReason is normal for a plain network
			// failure), turning "Script (): net::ERR_FAILED" into "Script (https://cdn...)".
			detail := failedURL
			if detail == "" {
				detail = string(e.BlockedReason)
			}
			msg := fmt.Sprintf("%s (%s): %s", e.Type, detail, e.ErrorText)
			// A CORS block reads as a bare net::ERR_FAILED; name the reason so a blocked
			// CDN module is told apart from a missing file.
			if cs := e.CorsErrorStatus; cs != nil && cs.CorsError != "" {
				msg += fmt.Sprintf(" (blocked by CORS: %s %s)", cs.CorsError, cs.FailedParameter)
			}
			c.add(kind, msg)
		case *network.EventResponseReceived:
			if e.Response != nil && e.Response.Status >= 400 &&
				(e.Type == network.ResourceTypeFetch || e.Type == network.ResourceTypeXHR || e.Type == network.ResourceTypeDocument) &&
				(probeHost == "" || hostOf(e.Response.URL) == probeHost) {
				c.mu.Lock()
				c.httpFails = append(c.httpFails, httpFail{id: e.RequestID, method: c.reqMethods[e.RequestID], url: e.Response.URL, status: e.Response.Status})
				c.mu.Unlock()
			}
			if e.Response != nil && (e.Type == network.ResourceTypeFetch || e.Type == network.ResourceTypeXHR) &&
				(probeHost == "" || hostOf(e.Response.URL) == probeHost) && isAPIPath(e.Response.URL) {
				c.mu.Lock()
				c.apiCalls = append(c.apiCalls, httpFail{id: e.RequestID, method: c.reqMethods[e.RequestID], url: e.Response.URL, status: e.Response.Status})
				if len(c.apiCalls) > maxAPICalls {
					c.apiCalls = c.apiCalls[len(c.apiCalls)-maxAPICalls:]
				}
				c.mu.Unlock()
			}
			// Record the first document-type response, that's the main frame.
			// Subsequent document responses (iframes, navigations) are ignored.
			if e.Type == network.ResourceTypeDocument && e.Response != nil {
				c.mu.Lock()
				if c.mainFrameStatus == 0 {
					c.mainFrameStatus = e.Response.Status
					c.mainFrameURL = e.Response.URL
				}
				c.mu.Unlock()
			}
		}
	})
	return c
}

func (c *pageErrorCollector) add(kind, msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	// Trim absurdly long stacks so report output stays readable.
	msg = stringutil.Truncate(msg, 600)
	c.mu.Lock()
	c.errs = append(c.errs, fmt.Sprintf("[%s] %s", kind, msg))
	c.mu.Unlock()
}

// addLog records one console line in the bounded tail, dropping the oldest when full.
func (c *pageErrorCollector) addLog(kind, msg string) {
	switch kind {
	case "log", "info", "warning", "debug":
	default:
		return
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	line := "[" + kind + "] " + stringutil.Truncate(msg, maxConsoleLine)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.logs); n > 0 && c.logs[n-1] == line {
		return // a repeated line adds nothing to a tail
	}
	c.logs = append(c.logs, line)
	if len(c.logs) > maxConsoleLogs {
		c.logs = c.logs[len(c.logs)-maxConsoleLogs:]
	}
}

// recentLogs returns up to n of the newest console lines, oldest first.
func (c *pageErrorCollector) recentLogs(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) > n {
		return append([]string(nil), c.logs[len(c.logs)-n:]...)
	}
	return append([]string(nil), c.logs...)
}

// snapshot returns a copy of everything collected so far.
func (c *pageErrorCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.errs...)
}

// take returns everything collected since the last take and clears it, so a
// multi-step flow attributes each error to the step it fired during.
func (c *pageErrorCollector) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.errs
	c.errs = nil
	return out
}

// takeHTTPFails returns the error responses recorded since the last call and clears
// them, so each flow step sees only the requests it caused.
func (c *pageErrorCollector) takeHTTPFails() []httpFail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.httpFails
	c.httpFails = nil
	return out
}

// takeAPICalls returns the API responses recorded since the last call and clears them.
func (c *pageErrorCollector) takeAPICalls() []httpFail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.apiCalls
	c.apiCalls = nil
	return out
}

// isAPIPath reports whether raw is a request to the project's own API.
func isAPIPath(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasPrefix(u.Path, "/api/")
}

// mainFrame returns the first document response's status and URL (0, "" if none yet).
func (c *pageErrorCollector) mainFrame() (int64, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mainFrameStatus, c.mainFrameURL
}

// splitHardSoft sorts tagged errors by impact: [uncaught] and [load_failed] are hard
// (a throw or a failed same-origin asset breaks the page); [console.error] is soft
// unless its stack points at project code; everything else is soft.
func splitHardSoft(collected []string) (hard, soft []string) {
	for _, e := range collected {
		switch {
		case strings.HasPrefix(e, "[uncaught]"), strings.HasPrefix(e, "[load_failed]"):
			hard = append(hard, e)
		case strings.HasPrefix(e, "[console.error]") && consoleErrorPointsAtProject(e):
			hard = append(hard, e)
		default:
			soft = append(soft, e)
		}
	}
	return hard, soft
}
