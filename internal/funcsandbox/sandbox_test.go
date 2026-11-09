package funcsandbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustCompile(t *testing.T, code string) *Handler {
	t.Helper()
	h, err := Compile(code)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return h
}

func req(method string) map[string]any {
	return map[string]any{"method": method, "path": "/api/fn", "query": map[string]any{}, "body": nil, "user": nil}
}

func decodeMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

func TestCompileRequiresHandler(t *testing.T) {
	if _, err := Compile(`function notHandler(){}`); err == nil {
		t.Fatal("expected error when handler() is missing")
	}
	if _, err := Compile(`function handler(req,ctx){ this is not js `); err == nil {
		t.Fatal("expected parse error for invalid JS")
	}
}

func TestCompileRejectsAsync(t *testing.T) {
	_, err := Compile(`async function handler(req, ctx){ return { body: {} }; }`)
	if err == nil {
		t.Fatal("expected an async handler to be rejected at compile")
	}
	if !strings.Contains(err.Error(), "async") {
		t.Fatalf("error should name async, got: %v", err)
	}
}

// TestPrepareIsolatesStateAcrossInvocations guards the program cache: the parsed
// *goja.Program is shared, but each Prepare must run it on a fresh VM so top-level
// state never leaks from one request into the next. A global counter incremented
// on each call must read 1 every time, not accumulate.
func TestPrepareIsolatesStateAcrossInvocations(t *testing.T) {
	const code = `var hits = 0; function handler(req, ctx){ hits += 1; return { hits: hits }; }`
	for i := 0; i < 3; i++ {
		h, err := Prepare(code)
		if err != nil {
			t.Fatalf("prepare #%d: %v", i, err)
		}
		resp, _, err := h.Run(req("GET"), Deps{})
		if err != nil {
			t.Fatalf("run #%d: %v", i, err)
		}
		if got := decodeMap(t, resp.Body)["hits"]; got != float64(1) {
			t.Fatalf("run #%d: expected fresh state (hits=1), got %v", i, got)
		}
	}
}

// TestPrepareDistinctSources confirms two different handler sources served through
// the shared cache don't cross-contaminate each other's behavior.
func TestPrepareDistinctSources(t *testing.T) {
	a, err := Prepare(`function handler(req, ctx){ return { who: "a" }; }`)
	if err != nil {
		t.Fatalf("prepare a: %v", err)
	}
	b, err := Prepare(`function handler(req, ctx){ return { who: "b" }; }`)
	if err != nil {
		t.Fatalf("prepare b: %v", err)
	}
	ra, _, _ := a.Run(req("GET"), Deps{})
	rb, _, _ := b.Run(req("GET"), Deps{})
	if decodeMap(t, ra.Body)["who"] != "a" || decodeMap(t, rb.Body)["who"] != "b" {
		t.Fatalf("sources crossed: a=%s b=%s", ra.Body, rb.Body)
	}
}

// TestPrepareRejectsAsync confirms the cached async detection still rejects async
// handlers on the hot path (Prepare), not just at save time.
func TestPrepareRejectsAsync(t *testing.T) {
	if _, err := Prepare(`async function handler(req, ctx){ return { body: {} }; }`); err == nil {
		t.Fatal("expected Prepare to reject an async handler")
	}
}

func TestEnvelopeResponse(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ return { status: 201, body: { ok: true }, headers: { "X-Test": "1" } }; }`)
	resp, _, err := h.Run(req("POST"), Deps{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("status = %d, want 201", resp.Status)
	}
	if decodeMap(t, resp.Body)["ok"] != true {
		t.Fatalf("body = %s, want ok:true", resp.Body)
	}
	if resp.Headers["X-Test"] != "1" {
		t.Fatalf("headers = %v, want X-Test:1", resp.Headers)
	}
}

func TestBareValueBody(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ return { hello: "world" }; }`)
	resp, _, err := h.Run(req("GET"), Deps{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if decodeMap(t, resp.Body)["hello"] != "world" {
		t.Fatalf("body = %s, want hello:world", resp.Body)
	}
}

func TestReqPassedThrough(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ return { m: req.method, p: req.path }; }`)
	resp, _, err := h.Run(req("DELETE"), Deps{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := decodeMap(t, resp.Body)
	if m["m"] != "DELETE" || m["p"] != "/api/fn" {
		t.Fatalf("req not passed through: %s", resp.Body)
	}
}

func TestQueryInjected(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ var rows = ctx.query("SELECT 1", []); return { n: rows.length, first: rows[0].name }; }`)
	deps := Deps{Query: func(sql string, params []any) ([]map[string]any, error) {
		return []map[string]any{{"name": "Ada"}, {"name": "Bo"}}, nil
	}}
	resp, _, err := h.Run(req("GET"), deps)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := decodeMap(t, resp.Body)
	if m["n"].(float64) != 2 || m["first"] != "Ada" {
		t.Fatalf("query result wrong: %s", resp.Body)
	}
}

// The critical goja assumption: a Go closure returning a non-nil error throws a
// catchable JS exception.
func TestQueryErrorThrowsAndIsCatchable(t *testing.T) {
	deps := Deps{Query: func(sql string, params []any) ([]map[string]any, error) {
		return nil, errors.New("no such table: widgets")
	}}

	// Uncaught -> Run returns an error.
	h := mustCompile(t, `function handler(req, ctx){ ctx.query("SELECT * FROM widgets", []); return {}; }`)
	if _, _, err := h.Run(req("GET"), deps); err == nil {
		t.Fatal("expected an uncaught ctx.query error to surface")
	}

	// Caught -> handler recovers and returns a 500 envelope.
	h2 := mustCompile(t, `function handler(req, ctx){ try { ctx.query("x", []); } catch (e) { return { status: 500, body: { error: String(e) } }; } return {}; }`)
	resp, _, err := h2.Run(req("GET"), deps)
	if err != nil {
		t.Fatalf("caught path should not error: %v", err)
	}
	if resp.Status != 500 || !strings.Contains(string(resp.Body), "widgets") {
		t.Fatalf("catch did not see the thrown error: status=%d body=%s", resp.Status, resp.Body)
	}
}

func TestWritesDeclared(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){
	  ctx.insert("orders", { total: 10 });
	  ctx.update("stock", { qty: { "$decrement": 1 } }, { id: 5 });
	  ctx.upsert("cells", { pos: "1,2", v: 9 }, ["pos"]);
	  return { ok: true };
	}`)
	_, writes, err := h.Run(req("POST"), Deps{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(writes) != 3 {
		t.Fatalf("want 3 writes, got %d", len(writes))
	}
	if writes[0].Op != "insert" || writes[0].Table != "orders" {
		t.Fatalf("write0 = %+v", writes[0])
	}
	if writes[1].Op != "update" || writes[1].Where["id"].(int64) != 5 {
		t.Fatalf("write1 = %+v", writes[1])
	}
	patch := writes[1].Row["qty"].(map[string]any)
	if patch["$decrement"].(int64) != 1 {
		t.Fatalf("update patch wrong: %+v", writes[1].Row)
	}
	if writes[2].Op != "upsert" || len(writes[2].Conflict) != 1 || writes[2].Conflict[0] != "pos" {
		t.Fatalf("write2 = %+v", writes[2])
	}
}

func TestWritesCapEnforced(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ for (var i=0;i<100;i++) ctx.insert("t", {i:i}); return {}; }`)
	if _, _, err := h.Run(req("POST"), Deps{}); err == nil {
		t.Fatalf("expected an error when a handler declares more than %d writes", MaxWrites)
	}
}

func TestFetchInjectedAndOptionsMapped(t *testing.T) {
	var gotURL, gotMethod, gotBody string
	deps := Deps{Fetch: func(url string, opts FetchOptions) (FetchResult, error) {
		gotURL, gotMethod, gotBody = url, opts.Method, opts.Body
		return FetchResult{Status: 200, Body: `{"temp":21}`, Headers: map[string]string{"Content-Type": "application/json"}}, nil
	}}
	h := mustCompile(t, `function handler(req, ctx){
	  var res = ctx.fetch("https://api.example.com/weather", { method: "POST", body: "city=NYC" });
	  return { s: res.status, b: res.body };
	}`)
	resp, _, err := h.Run(req("GET"), deps)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotURL != "https://api.example.com/weather" || gotMethod != "POST" || gotBody != "city=NYC" {
		t.Fatalf("fetch opts not mapped: url=%q method=%q body=%q", gotURL, gotMethod, gotBody)
	}
	m := decodeMap(t, resp.Body)
	if m["s"].(float64) != 200 || m["b"] != `{"temp":21}` {
		t.Fatalf("fetch result not read back: %s", resp.Body)
	}
}

func TestFetchCapEnforced(t *testing.T) {
	deps := Deps{Fetch: func(url string, opts FetchOptions) (FetchResult, error) {
		return FetchResult{Status: 200}, nil
	}}
	h := mustCompile(t, `function handler(req, ctx){ for (var i=0;i<50;i++) ctx.fetch("https://x/"+i, {}); return {}; }`)
	if _, _, err := h.Run(req("GET"), deps); err == nil {
		t.Fatalf("expected an error when a handler exceeds %d fetches", MaxFetches)
	}
}

func TestSecretInjected(t *testing.T) {
	deps := Deps{Secret: func(name string) (string, error) {
		if name != "STRIPE_KEY" {
			return "", errors.New("unknown secret")
		}
		return "sk_test_123", nil
	}}
	h := mustCompile(t, `function handler(req, ctx){ return { k: ctx.secret("STRIPE_KEY") }; }`)
	resp, _, err := h.Run(req("GET"), deps)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if decodeMap(t, resp.Body)["k"] != "sk_test_123" {
		t.Fatalf("secret not injected: %s", resp.Body)
	}
}

func TestUnavailableCapabilityThrows(t *testing.T) {
	// No Query closure injected -> ctx.query must throw, not panic.
	h := mustCompile(t, `function handler(req, ctx){ ctx.query("SELECT 1", []); return {}; }`)
	if _, _, err := h.Run(req("GET"), Deps{}); err == nil {
		t.Fatal("expected ctx.query to throw when no Query dep is injected")
	}
}

func TestTimeoutInterrupts(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ while(true){} }`)
	// Override the (now generous, I/O-oriented) default budget so the test is fast.
	_, _, err := h.Run(req("GET"), Deps{Budget: 200 * time.Millisecond})
	var to *Timeout
	if !errors.As(err, &to) {
		t.Fatalf("expected *Timeout for an infinite loop, got %v", err)
	}
}

// The budget must cover TOP-LEVEL code, not just the handler call: top-level
// runs on every request and a loop there never reaches handler(), so the
// interrupt armed around the handler call cannot save it. Un-interrupted, it
// pins a goroutine for the life of the process.
// Unbounded recursion in top-level code must come back as a catchable error from
// Compile. Without a call-stack cap on the validation VM it grows the Go stack
// until the whole process fatals.
func TestTopLevelRecursionIsBounded(t *testing.T) {
	_, err := Compile("function f(){ return f(); }\nf();\nfunction handler(req, ctx){ return {}; }")
	if err == nil {
		t.Fatal("expected unbounded top-level recursion to fail Compile")
	}
}

func TestTopLevelInfiniteLoopIsInterrupted(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := Compile("while(true){}\nfunction handler(req, ctx){ return {}; }")
		done <- err
	}()
	select {
	case err := <-done:
		var to *Timeout
		if !errors.As(err, &to) {
			t.Fatalf("expected *Timeout for a top-level infinite loop, got %v", err)
		}
	case <-time.After(topLevelBudget + 5*time.Second):
		t.Fatal("Compile hung on a top-level infinite loop: the wall-clock budget does not cover top-level code")
	}
}

func TestLLMInjectedAndOptionsMapped(t *testing.T) {
	var gotPrompt, gotSystem string
	var gotMax int
	deps := Deps{LLM: func(prompt, system string, maxTokens int) (string, error) {
		gotPrompt, gotSystem, gotMax = prompt, system, maxTokens
		return "3-bullet summary", nil
	}}
	h := mustCompile(t, `function handler(req, ctx){
	  return { s: ctx.llm("summarize: " + req.body.text, { system: "be terse", max_tokens: 128 }) };
	}`)
	resp, _, err := h.Run(map[string]any{"method": "POST", "body": map[string]any{"text": "hello"}}, deps)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotPrompt != "summarize: hello" || gotSystem != "be terse" || gotMax != 128 {
		t.Fatalf("ctx.llm opts not mapped: prompt=%q system=%q max=%d", gotPrompt, gotSystem, gotMax)
	}
	if decodeMap(t, resp.Body)["s"] != "3-bullet summary" {
		t.Fatalf("ctx.llm result not returned: %s", resp.Body)
	}
}

func TestLLMUnavailableThrows(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ return { s: ctx.llm("x") }; }`)
	if _, _, err := h.Run(req("GET"), Deps{}); err == nil {
		t.Fatal("expected ctx.llm to throw when no LLM dep is injected")
	}
}

func TestResponseSizeCapEnforced(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ var s = "x".repeat(300*1024); return { body: s }; }`)
	if _, _, err := h.Run(req("GET"), Deps{}); err == nil {
		t.Fatalf("expected an error when the response exceeds %d bytes", MaxResponseBytes)
	}
}

func TestNowInjected(t *testing.T) {
	h := mustCompile(t, `function handler(req, ctx){ return { t: ctx.now() }; }`)
	resp, _, err := h.Run(req("GET"), Deps{NowMS: 12345})
	if err != nil {
		t.Fatal(err)
	}
	if decodeMap(t, resp.Body)["t"].(float64) != 12345 {
		t.Fatalf("ctx.now() = %s, want 12345", resp.Body)
	}
}

// A `status` that is not an HTTP status code is a domain field, not an envelope:
// a build's `{ status: 'ok', files }` came back as a `null` body. A numeric status
// beside other keys keeps the code and sends the whole object; a real envelope is
// unchanged.
func TestStatusFieldIsNotAlwaysAnEnvelope(t *testing.T) {
	cases := []struct {
		name, ret  string
		wantStatus int
		wantKey    string
	}{
		{"domain status", `{ status: 'ok', files: [1, 2] }`, 200, "files"},
		{"numeric status with fields", `{ status: 404, reason: 'expired' }`, 404, "reason"},
		{"bare status only", `{ status: 204 }`, 204, ""},
		{"real envelope", `{ status: 201, body: { id: 7 } }`, 201, "id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := mustCompile(t, `function handler(req, ctx){ return `+c.ret+`; }`)
			resp, _, err := h.Run(req("GET"), Deps{})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if resp.Status != c.wantStatus {
				t.Errorf("status = %d, want %d", resp.Status, c.wantStatus)
			}
			if c.wantKey != "" {
				if _, ok := decodeMap(t, resp.Body)[c.wantKey]; !ok {
					t.Errorf("body %s lacks %q", resp.Body, c.wantKey)
				}
			}
		})
	}
}
