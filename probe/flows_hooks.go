package probe

// Steps that reach past the page. A person's journey through an app does not stop
// at the browser: a signup mails a confirmation link, a login asks for a 2FA code,
// an admin is someone the owner made one, a headless API is used with plain HTTP,
// a nightly job sends the digest, a payment provider calls a webhook. These steps
// cover that half: request/expectResponse drive and check the API directly (in the
// session's tab, so they act as that user), expectEmail reads what the app
// sent (kept, not delivered, during a replay), a flow's users start with the role
// or sign-in it needs, runSchedule and webhook fire the project's own automation.
// The project side is behind FlowHooks, which the tools and the build stage put on
// the context; without it those steps fail with a reason instead of guessing.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"openfabrica/internal/stringutil"
	"openfabrica/security"
)

// FlowHooks give a replay the project beyond its pages.
type FlowHooks interface {
	// Begin starts keeping the app's outgoing messages for this replay instead of
	// delivering them; end stops it.
	Begin() (end func())
	// Messages returns what the app sent since Begin, oldest first.
	Messages() []FlowMessage
	// SeedUser creates the account (replacing one left over with the same name)
	// and returns a session token for it.
	SeedUser(ctx context.Context, u FlowSeedUser) (token string, err error)
	// RunSchedule runs the scheduled task with this name now and waits for it.
	RunSchedule(ctx context.Context, name string) error
	// WebhookSignature signs body for the named inbound webhook (the value of its
	// X-Webhook-Signature header).
	WebhookSignature(name string, body []byte) (string, error)
}

// FlowMessage is an email or push notification the app sent.
type FlowMessage struct {
	Kind, To, Subject, Body string
}

// FlowSeedUser is an account a flow starts with.
type FlowSeedUser struct {
	Email, Username, Password, Role string
}

type flowHooksKeyT struct{}

// WithFlowHooks returns a context whose flow replays can use h.
func WithFlowHooks(ctx context.Context, h FlowHooks) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, flowHooksKeyT{}, h)
}

func flowHooksFrom(ctx context.Context) FlowHooks {
	h, _ := ctx.Value(flowHooksKeyT{}).(FlowHooks)
	return h
}

// flowResponse is the last response a session's request or webhook step got.
type flowResponse struct {
	Method, Path string
	Status       int    `json:"status"`
	Body         string `json:"body"`
}

func (r *flowResponse) label() string {
	return fmt.Sprintf("%s %s -> %d", r.Method, r.Path, r.Status)
}

// flowFetchJS performs one request from the page, as the app's own code would: same
// origin, the session's cookies, and - when the app keeps a session token in
// localStorage the way the platform's auth helper does - that token as the bearer.
//
//go:embed js/flow_fetch.js
var flowFetchJS string

// fetch sends one request from the session's tab. A session that has not opened a
// page yet (an API-only journey) opens the site's root first, so the request has
// the project's origin.
func (r *flowRun) fetch(ctx context.Context, s *flowSession, timeout time.Duration, method, path string, body any, headers map[string]string) (*flowResponse, *flowFailure) {
	target, ok := sitePath(path)
	if !ok {
		return nil, &flowFailure{"assert", fmt.Sprintf("%s %s is not a site path", method, path)}
	}
	var here string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`location.protocol`, &here))
	if !strings.HasPrefix(here, "http") {
		navCtx, cancel := context.WithTimeout(ctx, maxStepTimeout)
		err := chromedp.Run(navCtx, chromedp.Navigate(r.origin+"/"))
		cancel()
		if err != nil {
			return nil, &flowFailure{"navigation", fmt.Sprintf("could not open the site to send %s %s: %v", method, target, err)}
		}
	}
	args, err := json.Marshal([]any{method, target, body, headers})
	if err != nil {
		return nil, &flowFailure{"assert", fmt.Sprintf("the request body cannot be sent: %v", err)}
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var resp flowResponse
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	if err := chromedp.Run(c, chromedp.Evaluate(flowFetchJS+".apply(null, "+string(args)+")", &resp, awaitPromise)); err != nil {
		if c.Err() != nil {
			return nil, &flowFailure{"assert", fmt.Sprintf("%s %s did not answer within %s", method, target, timeout)}
		}
		return nil, &flowFailure{"navigation", fmt.Sprintf("%s %s failed: %v", method, target, err)}
	}
	resp.Method, resp.Path = method, target
	return &resp, nil
}

// checkStatus fails a response whose status is not the one the step names (any
// 2xx when it names none).
func checkStatus(resp *flowResponse, want *int) *flowFailure {
	ok := resp.Status >= 200 && resp.Status < 300
	expected := "a 2xx"
	if want != nil {
		ok = resp.Status == *want
		expected = strconv.Itoa(*want)
	}
	if ok {
		return nil
	}
	return &flowFailure{"assert", fmt.Sprintf("%s, expected %s: %s", resp.label(), expected, clipBody(resp.Body))}
}

func clipBody(b string) string {
	b = strings.Join(strings.Fields(b), " ")
	if b == "" {
		return "(empty body)"
	}
	return stringutil.Truncate(b, 300)
}

// expandAny substitutes variables in every string inside a request body.
func (r *flowRun) expandAny(v any, session string) any {
	switch t := v.(type) {
	case string:
		return r.vars.expand(t, session)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = r.expandAny(x, session)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = r.expandAny(x, session)
		}
		return out
	}
	return v
}

// decodeJSON parses a body keeping numbers exact.
func decodeJSON(body string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	return v, true
}

// matchResponse reports whether the response holds want: a string is looked for
// in the body text; anything else must be a subset of the JSON body (every key of
// an expected object present and matching, every element of an expected array
// matched by some element of the actual one).
func matchResponse(want any, body string) bool {
	if s, ok := want.(string); ok {
		return strings.Contains(strings.ToLower(body), strings.ToLower(s))
	}
	got, ok := decodeJSON(body)
	if !ok {
		return false
	}
	return jsonSubset(want, got)
}

func jsonSubset(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !jsonSubset(wv, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok {
			return false
		}
		for _, wv := range w {
			found := false
			for _, gv := range g {
				if jsonSubset(wv, gv) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	case nil:
		return got == nil
	}
	return scalarString(want) == scalarString(got)
}

// scalarString renders a JSON scalar for comparison and for remember: 3 and 3.0
// compare equal, strings are themselves.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// jsonPath walks "a.b.0.c" into a decoded JSON value.
func jsonPath(v any, path []string) (any, bool) {
	for _, p := range path {
		switch t := v.(type) {
		case map[string]any:
			x, ok := t[p]
			if !ok {
				return nil, false
			}
			v = x
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

var (
	flowLinkRe = regexp.MustCompile(`https?://[^\s"'<>)]+`)
	flowCodeRe = regexp.MustCompile(`\b\d{4,8}\b`)
	flowTagRe  = regexp.MustCompile(`<[^>]*>`)
)

// messageText is a message as a person reads it: subject and body, tags removed.
func messageText(m FlowMessage) string {
	return strings.Join(strings.Fields(m.Subject+" "+flowTagRe.ReplaceAllString(m.Body, " ")), " ")
}

// rememberSpecial reads a remember step's value from somewhere other than the page:
// the session's last response (response.<path>) or last matched message
// (email.link, email.code, ...). ok is false when from is a page element.
func (r *flowRun) rememberSpecial(s *flowSession, from string) (value string, ok bool, fail *flowFailure) {
	if !flowSpecialFromRe.MatchString(from) {
		return "", false, nil
	}
	parts := strings.Split(from, ".")
	switch parts[0] {
	case "response":
		if s.lastResp == nil {
			return "", true, &flowFailure{"assert", "there is no response to read yet"}
		}
		if len(parts) == 1 {
			return strings.TrimSpace(s.lastResp.Body), true, nil
		}
		doc, isJSON := decodeJSON(s.lastResp.Body)
		if !isJSON {
			return "", true, &flowFailure{"assert", fmt.Sprintf("the last response (%s) is not JSON, so it has no %s: %s", s.lastResp.label(), from, clipBody(s.lastResp.Body))}
		}
		v, found := jsonPath(doc, parts[1:])
		if !found || v == nil {
			return "", true, &flowFailure{"assert", fmt.Sprintf("the last response (%s) has no %s: %s", s.lastResp.label(), from, clipBody(s.lastResp.Body))}
		}
		return scalarString(v), true, nil
	}
	kind, what := parts[0], parts[1]
	m := s.lastMsg
	if m == nil || m.Kind != kind {
		return "", true, &flowFailure{"assert", fmt.Sprintf("from %q reads the message an earlier expectEmail step found in this session, and there is none", from)}
	}
	text := messageText(*m)
	var v string
	switch what {
	case "link":
		v = flowLinkRe.FindString(m.Body)
		if v == "" {
			v = flowLinkRe.FindString(m.Subject)
		}
		v = strings.TrimRight(v, ".,;")
	case "code":
		v = flowCodeRe.FindString(flowTagRe.ReplaceAllString(m.Body, " "))
		if v == "" {
			v = flowCodeRe.FindString(m.Subject)
		}
	case "subject":
		v = m.Subject
	default:
		v = stringutil.Truncate(text, 1000)
	}
	if v == "" {
		return "", true, &flowFailure{"assert", fmt.Sprintf("the %s to %s has no %s: %q", kind, m.To, what, stringutil.Truncate(text, 300))}
	}
	return v, true, nil
}

// expectMessage waits for an email to the recipient whose text contains want.
func (r *flowRun) expectMessage(ctx context.Context, s *flowSession, kind, want, to string, timeout time.Duration) *flowFailure {
	if r.hooks == nil {
		return &flowFailure{"navigation", "this replay cannot see what the app sends (no project inbox here), so expectEmail cannot run"}
	}
	fold := func(x string) string { return strings.ToLower(strings.Join(strings.Fields(x), " ")) }
	deadline := time.Now().Add(timeout)
	for {
		msgs := r.hooks.Messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.Kind == kind && strings.Contains(strings.ToLower(m.To), strings.ToLower(to)) && strings.Contains(fold(messageText(m)), fold(want)) {
				s.lastMsg = &m
				return nil
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			var seen []string
			for _, m := range msgs {
				if m.Kind == kind && len(seen) < 4 {
					seen = append(seen, fmt.Sprintf("to %s: %q", m.To, stringutil.Truncate(messageText(m), 120)))
				}
			}
			detail := fmt.Sprintf("no %s to %s containing %q arrived (waited %s)", kind, to, want, timeout)
			if len(seen) == 0 {
				detail += fmt.Sprintf("; the app sent no %s at all during this replay - check what should send it (a trigger, the magic-link endpoint, a function) and that the %s provider is configured", kind, kind)
			} else {
				detail += "; it sent: " + strings.Join(seen, "; ")
			}
			return &flowFailure{"assert", detail}
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(250*time.Millisecond))
	}
}

// flowPhone is a session's {{phone}}: a fictional +1 202 555-01xx number, stable
// for the session within a run.
func flowPhone(run, session string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(run + "/" + session))
	return fmt.Sprintf("+1202555%04d", 100+h.Sum32()%100)
}

// flowTOTP computes the current 2FA code for a remembered secret: the base32 text
// an app shows, or the otpauth:// link it encodes in its QR code.
func flowTOTP(secret string) string {
	secret = strings.TrimSpace(secret)
	if u, err := url.Parse(secret); err == nil && u.Scheme == "otpauth" {
		secret = u.Query().Get("secret")
	}
	secret = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(secret, " ", ""), "-", ""))
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(secret, "="))
	if err != nil || len(raw) == 0 {
		return "invalid-2fa-secret"
	}
	return security.TOTPCode(raw, time.Now().Unix()/30)
}

// webhookBody is the exact bytes a webhook step sends (and signs).
func webhookBody(body any) []byte {
	switch t := body.(type) {
	case nil:
		return []byte("{}")
	case string:
		return []byte(t)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
	return bytes.TrimRight(buf.Bytes(), "\n")
}
