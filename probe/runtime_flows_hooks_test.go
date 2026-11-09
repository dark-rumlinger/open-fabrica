package probe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"openfabrica/security"
	"openfabrica/store"
)

// fakeFlowHooks stands in for the project: an outbox the test app writes to, seeded
// users as tokens, one scheduled task, and a webhook secret.
type fakeFlowHooks struct {
	mu     sync.Mutex
	msgs   []FlowMessage
	began  int
	seeded []FlowSeedUser
}

func (h *fakeFlowHooks) Begin() func() {
	h.mu.Lock()
	h.began++
	h.mu.Unlock()
	return func() {}
}

func (h *fakeFlowHooks) send(m FlowMessage) {
	h.mu.Lock()
	h.msgs = append(h.msgs, m)
	h.mu.Unlock()
}

func (h *fakeFlowHooks) Messages() []FlowMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]FlowMessage(nil), h.msgs...)
}

func (h *fakeFlowHooks) SeedUser(_ context.Context, u FlowSeedUser) (string, error) {
	h.mu.Lock()
	h.seeded = append(h.seeded, u)
	h.mu.Unlock()
	return "tok-" + u.Role, nil
}

func (h *fakeFlowHooks) RunSchedule(_ context.Context, name string) error {
	if name != "digest" {
		return errors.New("no scheduled task named " + name)
	}
	h.send(FlowMessage{Kind: "email", To: "owner@example.com", Subject: "Daily digest", Body: "3 new orders"})
	return nil
}

const fakeWebhookSecret = "whsec"

func fakeSign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(fakeWebhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *fakeFlowHooks) WebhookSignature(name string, body []byte) (string, error) {
	if name != "payments" {
		return "", errors.New("no inbound webhook named " + name)
	}
	return fakeSign(body), nil
}

const flowHooksTOTPSecret = "JBSWY3DPEHPK3PXP"

// flowAccountPage signs up (mailing a confirmation link and code), turns on 2FA
// with an authenticator code, and shows the stored token.
const flowAccountPage = `<!doctype html><html><body>
<input id="email"><button id="signup">Sign up</button><p id="status"></p>
<p>Secret: <code id="secret">` + flowHooksTOTPSecret + `</code></p><input id="code"><button id="enable2fa">Enable 2FA</button><p id="twofa"></p>
<p id="token"></p>
<script>
document.getElementById('token').textContent = 'token ' + (localStorage.getItem('auth_token') || 'none');
function post(path, body) { return fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }).then(function (r) { return r.json(); }); }
document.getElementById('signup').onclick = function () {
  post('/api/signup', { email: document.getElementById('email').value }).then(function () { document.getElementById('status').textContent = 'Check your inbox'; });
};
document.getElementById('enable2fa').onclick = function () {
  post('/api/totp', { code: document.getElementById('code').value }).then(function (r) { document.getElementById('twofa').textContent = r.ok ? '2FA on' : '2FA code wrong'; });
};
</script></body></html>`

func runHooksFlow(t *testing.T, hooks *fakeFlowHooks, src string) FlowResult {
	t.Helper()
	var paid string
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	var base string
	mux.HandleFunc("/account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(flowAccountPage))
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><h1>Verified " + r.URL.Query().Get("t") + "</h1>"))
	})
	mux.HandleFunc("/api/signup", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Email string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		hooks.send(FlowMessage{Kind: "email", To: in.Email, Subject: "Confirm your account",
			Body: `<p>Your code is <b>482913</b>. Or <a href="` + base + `/verify?t=abc">confirm here</a>.</p>`})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/totp", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		writeJSON(w, 200, map[string]bool{"ok": security.VerifyTOTPCode(flowHooksTOTPSecret, in.Code, time.Now())})
	})
	mux.HandleFunc("/api/orders", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["name"] == nil || in["name"] == "" {
			writeJSON(w, 422, map[string]string{"error": "name is required"})
			return
		}
		writeJSON(w, 201, map[string]any{"id": 7, "name": in["name"], "status": "queued"})
	})
	mux.HandleFunc("/api/orders/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": 7, "status": "queued", "paid": paid != "", "items": []any{map[string]any{"sku": "a", "qty": 1}, map[string]any{"sku": "b", "qty": 2}}})
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"auth": r.Header.Get("Authorization")})
	})
	mux.HandleFunc("/webhooks/payments", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Webhook-Signature") != fakeSign(body) {
			writeJSON(w, 401, map[string]string{"error": "bad signature"})
			return
		}
		paid = string(body)
		writeJSON(w, 200, map[string]bool{"success": true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	suite, err := ParseFlowSource(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := context.Background()
	if hooks != nil {
		ctx = WithFlowHooks(ctx, hooks)
	}
	res := RunFlowTests(ctx, suite, srv.URL, "", "")
	if res.Skipped != "" {
		t.Fatalf("the replay was skipped although Chrome launched: %s", res.Skipped)
	}
	return res
}

// A headless API journey: create, read back, check, chain the id, and a 4xx that
// is the point of the step.
func TestFlowsHooks_APIJourney(t *testing.T) {
	res := runHooksFlow(t, &fakeFlowHooks{}, `export default { flows: [
		{ name: "orders api", steps: [
			{ request: "POST /api/orders", body: { name: "Order {{run}}" }, status: 201 },
			{ expectResponse: { status: "queued", name: "Order {{run}}" } },
			{ remember: "orderId", from: "response.id" },
			{ request: "GET /api/orders/{{orderId}}" },
			{ expectResponse: { items: [{ sku: "b", qty: 2 }] } },
			{ remember: "sku", from: "response.items.1.sku" },
			{ expectJs: "true" },
			{ request: "POST /api/orders", body: {}, status: 422 },
			{ expectResponse: "name is required" },
		] },
		{ name: "wrong status", steps: [{ request: "POST /api/orders", body: {} }] },
	] }`)
	if len(res.Passed) != 1 || res.Passed[0] != "orders api" {
		t.Fatalf("passed = %v issues = %+v", res.Passed, res.Issues)
	}
	wantMessage(t, res, "POST /api/orders -> 422, expected a 2xx", "name is required")
}

// A signup mails a confirmation; the flow reads it, opens its link and uses its
// code. The recipient is the session's own {{email}}.
func TestFlowsHooks_InboxLinkAndCode(t *testing.T) {
	hooks := &fakeFlowHooks{}
	res := runHooksFlow(t, hooks, `export default { flows: [{ name: "confirm", steps: [
		{ goto: "/account" },
		{ fill: "#email", value: "{{email}}" },
		{ click: "#signup" },
		{ expectEmail: "Confirm your account" },
		{ remember: "code", from: "email.code" },
		{ remember: "link", from: "email.link" },
		{ goto: "{{link}}" },
		{ expectText: "Verified abc" },
		{ expectJs: "'{{code}}' === '482913'" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("inbox: %s", res.Issues[0].Message)
	}
	if hooks.began == 0 {
		t.Fatal("the replay must open the outbox")
	}
}

func TestFlowsHooks_MissingEmailExplains(t *testing.T) {
	res := runHooksFlow(t, &fakeFlowHooks{}, `export default { flows: [{ name: "no mail", steps: [
		{ goto: "/account" },
		{ expectEmail: "Welcome", timeout: 600 },
	] }] }`)
	wantMessage(t, res, "no email to of-test+", `containing "Welcome"`, "the app sent no email at all")
}

// 2FA from a remembered secret.
func TestFlowsHooks_TOTP(t *testing.T) {
	res := runHooksFlow(t, &fakeFlowHooks{}, `export default { flows: [{ name: "second factors", steps: [
		{ goto: "/account" },
		{ remember: "secret", from: "#secret" },
		{ fill: "#code", value: "{{totp:secret}}" },
		{ click: "#enable2fa" },
		{ expectText: "2FA on", in: "#twofa" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("second factors: %s", res.Issues[0].Message)
	}
}

// A flow's users start with their role, and a signedIn one with its token, which
// requests then carry as the app's own code would.
func TestFlowsHooks_SeededUsers(t *testing.T) {
	hooks := &fakeFlowHooks{}
	res := runHooksFlow(t, hooks, `export default { flows: [{ name: "admin", users: { admin: { role: "admin", signedIn: true }, bob: { role: "member" } }, steps: [
		{ goto: "/account", as: "admin" },
		{ expectText: "token tok-admin", as: "admin" },
		{ request: "GET /api/me", as: "admin" },
		{ expectResponse: { auth: "Bearer tok-admin" }, as: "admin" },
		{ goto: "/account", as: "bob" },
		{ expectText: "token none", as: "bob" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("seeded users: %s", res.Issues[0].Message)
	}
	if len(hooks.seeded) != 2 || hooks.seeded[0].Role != "admin" || !strings.Contains(hooks.seeded[0].Email, "-admin@") || hooks.seeded[1].Role != "member" {
		t.Fatalf("seeded = %+v", hooks.seeded)
	}
}

// Automation: a scheduled task's mail to the owner, and a signed inbound webhook
// whose effect the API then shows.
func TestFlowsHooks_ScheduleAndWebhook(t *testing.T) {
	res := runHooksFlow(t, &fakeFlowHooks{}, `export default { flows: [
		{ name: "digest", steps: [{ runSchedule: "digest" }, { expectEmail: "Daily digest", to: "owner@example.com" }, { remember: "digest", from: "email.body" }] },
		{ name: "payment", steps: [
			{ webhook: "payments", body: { type: "payment.completed", order: 7 } },
			{ request: "GET /api/orders/7" },
			{ expectResponse: { paid: true } },
		] },
		{ name: "unknown task", steps: [{ runSchedule: "nightly" }] },
	] }`)
	if strings.Join(res.Passed, ",") != "digest,payment" {
		t.Fatalf("passed = %v issues = %+v", res.Passed, res.Issues)
	}
	wantMessage(t, res, `scheduled task "nightly": no scheduled task named nightly`)
}

// Without project access (no hooks) the steps that need it say so.
func TestFlowsHooks_WithoutHooks(t *testing.T) {
	res := runHooksFlow(t, nil, `export default { flows: [{ name: "a", steps: [{ goto: "/account" }, { expectEmail: "x" }] }] }`)
	wantMessage(t, res, "cannot see what the app sends")
}
