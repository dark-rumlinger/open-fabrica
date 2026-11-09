// Package funcsandbox runs a model-authored HTTP/event handler in a locked-down
// goja runtime with no DB/network/hub dependency (handlers touch only the
// closures injected on Deps). Unlike the room reducer it allows synchronous DB
// reads (ctx.query) and outbound HTTP (ctx.fetch) on a larger time budget.
//
// A handler is a single synchronous function:
//
//	function handler(req, ctx) { ...; return { status, body, headers }; }
//
// It must NOT be async: ctx.query/fetch/secret are synchronous and there is no
// event loop to resolve a returned Promise. Compile rejects an async handler at save time.
package funcsandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Budget is the DEFAULT wall-clock cap on a single handler invocation (a per-endpoint
// timeout_ms overrides it via Deps.Budget). A handler that exceeds it is interrupted so
// one request fails instead of pinning a goroutine. Generous because functions are
// I/O-oriented; each I/O closure also carries its own per-call timeout.
const Budget = 30 * time.Second

// topLevelBudget caps the module's TOP-LEVEL code (everything outside handler()), which
// runs once per compile AND once per request. Far tighter than Budget because top-level
// code has no I/O and only defines functions/constants; seconds out there is
// pathological and re-paid every request.
const topLevelBudget = 2 * time.Second

// withBudget runs fn with vm's wall-clock interrupt armed, mapping goja's interrupt to
// *Timeout. EVERY path that executes model-authored JS must run inside one - top-level
// module code as much as the handler call - or a `while(true){}` at top level pins the
// goroutine forever, un-interruptibly.
func withBudget(vm *goja.Runtime, budget time.Duration, fn func() error) error {
	stop := time.AfterFunc(budget, func() { vm.Interrupt("timeout") })
	defer stop.Stop()
	defer vm.ClearInterrupt()
	err := fn()
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		return &Timeout{}
	}
	return err
}

// Caps bound what one invocation can produce (goja has no hard heap cap). They
// cap OUTPUT, not intermediate allocation: an unbounded accumulating loop can
// still OOM the process before any cap or timeout fires, so run under an OS or
// container memory limit + auto-restart (and set GOMEMLIMIT). See docs/SECURITY.md.
const (
	MaxResponseBytes = 256 * 1024 // serialized response body
	MaxWrites        = 32         // declared DB writes per invocation
	MaxQueries       = 64         // ctx.query calls per invocation
	MaxFetches       = 8          // ctx.fetch calls per invocation
	MaxLLMCalls      = 4          // ctx.llm calls per invocation (each is a real, billable completion)
	MaxCheckouts     = 3          // ctx.checkout calls per invocation (each opens a real provider session)
)

// Write is a durable DB write the handler declared via ctx.insert/update/upsert. Like
// the room reducer, a write is a DECLARATION applied by the caller after the handler
// returns (the sandbox holds no DB handle), so the handler holds no live transaction.
// The shape mirrors roomactor.Write so the caller can apply both through one builder.
type Write struct {
	Op       string         // "insert" | "update" | "upsert"
	Table    string         // target table
	Row      map[string]any // insert/upsert row, or update SET patch ({"$increment":n} supported)
	Where    map[string]any // update match (nil otherwise)
	Conflict []string       // upsert conflict-key columns (nil otherwise)
}

// Response is the HTTP response a handler returns. A handler that returns a bare
// value (not an envelope) yields Status 200 with that value as Body.
type Response struct {
	Status  int
	Body    json.RawMessage
	Headers map[string]string
}

// FetchOptions is the second argument the handler passes to ctx.fetch.
type FetchOptions struct {
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// FetchResult is what ctx.fetch resolves to. Body is the (bounded) response text.
type FetchResult struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// LLMOptions is the optional second argument to ctx.llm(prompt, opts).
type LLMOptions struct {
	System    string `json:"system"`     // optional system prompt
	MaxTokens int    `json:"max_tokens"` // response cap (0 = a sensible default)
}

// Deps are the runtime capabilities the caller injects for one invocation. Each is
// nil-safe: a nil closure surfaces to the handler as a thrown "not available" error
// rather than a Go panic, so an unconfigured capability fails cleanly (a 500).
type Deps struct {
	// Query runs a read-only SELECT and returns the rows. The caller enforces the
	// SELECT-only / no-system-table / no-PASSWORD-column guard and the row cap.
	Query func(sql string, params []any) ([]map[string]any, error)
	// Fetch performs a single SSRF-guarded outbound request.
	Fetch func(url string, opts FetchOptions) (FetchResult, error)
	// Secret resolves a named secret to its decrypted value (never logged).
	Secret func(name string) (string, error)
	// LLM runs one completion against the project's configured provider and returns the
	// text, so a handler can summarize/classify/extract. Nil when no provider is
	// configured (ctx.llm then throws "not available").
	LLM func(prompt, system string, maxTokens int) (string, error)
	// Checkout opens a hosted payment checkout ({amount, success_url, cancel_url,
	// description?, currency?, metadata?}) and returns {checkout_url, payment_id, ...}.
	// Nil when no payments implementation is linked (ctx.checkout then throws).
	Checkout func(args map[string]any) (map[string]any, error)
	// NowMS is the clock ctx.now() returns (Unix milliseconds).
	NowMS int64
	// Budget overrides the wall-clock cap for this invocation (0 = package default),
	// letting a per-endpoint timeout_ms widen the window for a slow external call.
	Budget time.Duration
}

// Handler is a compiled, validated handler(req, ctx) in a locked-down runtime.
// It is NOT safe for concurrent use: each invocation must own its own runtime,
// so callers build one per request. Cheap because the parsed program is cached (getProgram).
type Handler struct {
	vm *goja.Runtime
	fn goja.Callable
}

// asyncCheckSrc detects whether the defined handler is an async function. Compiled and
// run once per distinct source (in getProgram), not per request.
const asyncCheckSrc = `(function(){ try { return handler.constructor && handler.constructor.name === 'AsyncFunction'; } catch(e){ return false; } })()`

// cachedProg is the parsed, immutable program for a handler source plus facts derived
// at parse time. A *goja.Program is safe to share across runtimes (unlike a Runtime),
// so caching it removes re-parsing and async-detection from the per-request path.
type cachedProg struct {
	prog  *goja.Program
	async bool
}

// maxCachedPrograms bounds the program cache. Handler sources change as the model edits
// functions, so keying on source would otherwise grow without limit. When the cap is
// hit we drop the whole cache; entries recompile lazily. 512 covers a project's live
// endpoints plus recent edits.
const maxCachedPrograms = 512

var (
	progMu    sync.RWMutex
	progCache = map[string]cachedProg{}
)

// errNoHandlerFunc is returned when source parses but defines no handler. A sentinel
// so callers classify it with errors.Is instead of matching the message text.
var errNoHandlerFunc = errors.New("must define a function handler(req, ctx)")

// getProgram parses code into a shareable *goja.Program and determines async-ness,
// memoizing both keyed on the source. The parse and the async-detection run at
// most once per distinct handler; every later request reuses the result.
func getProgram(code string) (cachedProg, error) {
	progMu.RLock()
	cp, ok := progCache[code]
	progMu.RUnlock()
	if ok {
		return cp, nil
	}

	prog, err := goja.Compile("handler", code, false)
	if err != nil {
		return cachedProg{}, fmt.Errorf("handler does not parse: %w", err)
	}
	// Run on a throwaway VM once to validate the shape and detect async, so the hot path
	// never compiles or runs the detection IIFE. Both execute model-authored code (the
	// IIFE reaches through handler.constructor, which authored code can trap), so both
	// go under the budget.
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	// Same call-stack bound as build: top-level recursion here would otherwise grow
	// the Go stack until the process fatals.
	vm.SetMaxCallStackSize(1000)
	var async bool
	if err := withBudget(vm, topLevelBudget, func() error {
		if _, err := vm.RunProgram(prog); err != nil {
			return err
		}
		if _, ok := goja.AssertFunction(vm.Get("handler")); !ok {
			return errNoHandlerFunc
		}
		if v, err := vm.RunString(asyncCheckSrc); err == nil && v != nil {
			async = v.ToBoolean()
		}
		return nil
	}); err != nil {
		// A budget overrun and a missing handler are already precise; only a raw
		// goja throw needs the "does not parse" framing.
		var to *Timeout
		if errors.As(err, &to) || errors.Is(err, errNoHandlerFunc) {
			return cachedProg{}, err
		}
		return cachedProg{}, fmt.Errorf("handler does not parse: %w", err)
	}
	entry := cachedProg{prog: prog, async: async}

	progMu.Lock()
	if len(progCache) >= maxCachedPrograms {
		progCache = map[string]cachedProg{}
	}
	progCache[code] = entry
	progMu.Unlock()
	return entry, nil
}

// build constructs a fresh, isolated runtime for one invocation from the cached program
// and binds handler(req, ctx). A goja.Runtime is not goroutine-safe, so every invocation
// owns its own VM; only the parsed *goja.Program is shared. Locked-down VM: no
// require/fs/net/process.
func build(code string) (*Handler, error) {
	cp, err := getProgram(code)
	if err != nil {
		return nil, err
	}
	// Reject an async handler: ctx.query/fetch/secret are synchronous and there is no
	// loop to resolve a returned Promise, so an async handler would return a pending
	// Promise and every response would be empty. (Async-ness is cached from getProgram.)
	if cp.async {
		return nil, errors.New("handler must not be async: ctx.query/fetch/secret are synchronous - remove async/await and return the response object directly")
	}
	vm := goja.New()
	// Map Go struct fields to their json-tag names in BOTH directions, so a JS
	// handler passes {method,body} to ctx.fetch and reads {status,headers,body}
	// off the result (FetchOptions/FetchResult), not the capitalized Go names.
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	// Bound the JS call stack so runaway recursion returns a catchable error instead
	// of growing the Go stack until the process fatals (uncatchable, and it would
	// take down every project in this single binary). Parity with the room reducer.
	vm.SetMaxCallStackSize(1000)
	// Per-request top-level run: getProgram validated this source once, but the program
	// re-executes here every invocation and top-level code can be non-deterministic
	// (Date/Math.random are left in place), so terminating at compile time isn't now.
	if err := withBudget(vm, topLevelBudget, func() error {
		_, err := vm.RunProgram(cp.prog)
		return err
	}); err != nil {
		var to *Timeout
		if errors.As(err, &to) {
			return nil, err
		}
		return nil, fmt.Errorf("handler does not parse: %w", err)
	}
	fn, ok := goja.AssertFunction(vm.Get("handler"))
	if !ok {
		return nil, errNoHandlerFunc
	}
	return &Handler{vm: vm, fn: fn}, nil
}

// Compile parses and fully validates handler source, returning a ready-to-run Handler.
// Used at save time (validation) and request time (execution), so "valid on save" ==
// "valid at runtime". The program is cached, so this is cheap per request; see Prepare
// for the hot-path alias.
func Compile(code string) (*Handler, error) {
	return build(code)
}

// Prepare builds a ready-to-run handler for one invocation. The hot-path entry point:
// identical to Compile, but named to signal the caller is preparing an isolated runtime
// for a single request (reusing the cached program), not validating new source.
func Prepare(code string) (*Handler, error) {
	return build(code)
}

// Run invokes the handler for one request. req is the request envelope
// (method/path/query/headers/body/user); deps injects the I/O capabilities. Returns the
// response and any declared writes, or an error (a thrown exception, budget overrun, or
// non-serializable/oversized response) the caller maps to a 500.
func (h *Handler) Run(req map[string]any, deps Deps) (resp *Response, writes []Write, err error) {
	var (
		queries  int
		fetches  int
		llmCalls int
	)

	ctx := h.vm.NewObject()
	// ctx.now() is the injected clock; ctx.random() is a convenience (Math.random
	// is also available - a function needs no determinism, unlike the reducer).
	_ = ctx.Set("now", func() int64 { return deps.NowMS })
	_ = ctx.Set("random", func() float64 { return rand.Float64() })

	// ctx.query(sql, params?) -> rows. Synchronous read; the injected closure owns
	// the SELECT-only guard and row cap. Returning an error makes goja throw it,
	// so the handler can try/catch or let it 500.
	_ = ctx.Set("query", func(sql string, params []any) (any, error) {
		if deps.Query == nil {
			return nil, errors.New("ctx.query is not available")
		}
		queries++
		if queries > MaxQueries {
			return nil, fmt.Errorf("ctx.query call limit (%d) exceeded", MaxQueries)
		}
		rows, qerr := deps.Query(sql, params)
		if qerr != nil {
			return nil, qerr
		}
		return rows, nil
	})

	// ctx.insert/update/upsert DECLARE a durable write, applied by the caller after
	// a successful return. Enforce the write cap eagerly so a runaway loop fails
	// fast rather than growing an unbounded slice before a post-hoc check.
	declare := func(w Write) error {
		if len(writes) >= MaxWrites {
			return fmt.Errorf("declared more than %d DB writes", MaxWrites)
		}
		writes = append(writes, w)
		return nil
	}
	_ = ctx.Set("insert", func(table string, row map[string]any) error {
		return declare(Write{Op: "insert", Table: table, Row: row})
	})
	_ = ctx.Set("update", func(table string, patch, where map[string]any) error {
		return declare(Write{Op: "update", Table: table, Row: patch, Where: where})
	})
	_ = ctx.Set("upsert", func(table string, row map[string]any, conflict []string) error {
		return declare(Write{Op: "upsert", Table: table, Row: row, Conflict: conflict})
	})

	// ctx.fetch(url, opts?) -> {status, headers, body}. Synchronous, SSRF-guarded
	// by the injected closure; bounded per invocation.
	_ = ctx.Set("fetch", func(url string, opts *FetchOptions) (any, error) {
		if deps.Fetch == nil {
			return nil, errors.New("ctx.fetch is not available")
		}
		fetches++
		if fetches > MaxFetches {
			return nil, fmt.Errorf("ctx.fetch call limit (%d) exceeded", MaxFetches)
		}
		var o FetchOptions
		if opts != nil {
			o = *opts
		}
		res, ferr := deps.Fetch(url, o)
		if ferr != nil {
			return nil, ferr
		}
		return res, nil
	})

	// ctx.secret(name) -> decrypted value. Never logged by the sandbox.
	_ = ctx.Set("secret", func(name string) (string, error) {
		if deps.Secret == nil {
			return "", errors.New("ctx.secret is not available")
		}
		return deps.Secret(name)
	})

	// ctx.checkout({amount, success_url, cancel_url, description?, currency?, metadata?})
	// -> {checkout_url, payment_id, external_id, amount, currency}. Opens a hosted
	// checkout on the project's configured payment provider and records the payment row
	// the payment webhook reconciles. Bounded per invocation like the other outbound calls.
	checkouts := 0
	_ = ctx.Set("checkout", func(args map[string]any) (map[string]any, error) {
		if deps.Checkout == nil {
			return nil, errors.New("ctx.checkout is not available")
		}
		checkouts++
		if checkouts > MaxCheckouts {
			return nil, fmt.Errorf("ctx.checkout call limit (%d) exceeded", MaxCheckouts)
		}
		return deps.Checkout(args)
	})

	// ctx.llm(prompt, opts?) -> completion text from the project's configured
	// provider. Synchronous and bounded per invocation (each call is billable).
	_ = ctx.Set("llm", func(prompt string, opts *LLMOptions) (string, error) {
		if deps.LLM == nil {
			return "", errors.New("ctx.llm is not available: no LLM provider is configured for this project")
		}
		llmCalls++
		if llmCalls > MaxLLMCalls {
			return "", fmt.Errorf("ctx.llm call limit (%d) exceeded", MaxLLMCalls)
		}
		var o LLMOptions
		if opts != nil {
			o = *opts
		}
		return deps.LLM(prompt, o.System, o.MaxTokens)
	})

	budget := Budget
	if deps.Budget > 0 {
		budget = deps.Budget
	}
	var ret goja.Value
	if gerr := withBudget(h.vm, budget, func() error {
		var err error
		ret, err = h.fn(goja.Undefined(), h.vm.ToValue(req), ctx)
		return err
	}); gerr != nil {
		var to *Timeout
		if errors.As(gerr, &to) {
			return nil, nil, gerr
		}
		return nil, nil, fmt.Errorf("handler threw: %w", gerr)
	}

	// A returned Promise (async handler that slipped past Compile, or an explicit one)
	// has a then() method and cannot be resolved without a loop - reject it.
	if obj, ok := ret.(*goja.Object); ok {
		if _, ok := goja.AssertFunction(obj.Get("then")); ok {
			return nil, nil, errors.New("handler returned a Promise: it must be synchronous (no async/await)")
		}
	}

	r, perr := parseResponse(ret)
	if perr != nil {
		return nil, nil, perr
	}
	if len(r.Body) > MaxResponseBytes {
		return nil, nil, fmt.Errorf("response body is %d bytes (limit %d)", len(r.Body), MaxResponseBytes)
	}
	return r, writes, nil
}

// Timeout is returned when an invocation exceeds Budget.
type Timeout struct{}

func (e *Timeout) Error() string { return "handler exceeded time budget" }

// parseResponse turns the handler's return value into a Response. An object carrying
// body or headers is an explicit envelope; so is one whose status is an HTTP status
// code. A `status` that is not one (`{status: 'ok', files: [...]}`, a domain field)
// makes the object a bare value: reading it as an envelope served `null` for every
// such reply. A numeric status beside other keys and no body keeps the
// status and sends the whole object as the body. Anything else (bare object, array,
// string, number) becomes a 200 with that value as the JSON body.
func parseResponse(v goja.Value) (*Response, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return &Response{Status: 200, Body: json.RawMessage("null")}, nil
	}
	exported := v.Export()
	m, isMap := exported.(map[string]any)
	status, statusOK := 0, false
	if isMap {
		status, statusOK = httpStatus(m["status"])
	}
	if isMap && !hasKey(m, "body") && !hasKey(m, "headers") && statusOK && len(m) > 1 {
		b, err := marshalBody(exported)
		if err != nil {
			return nil, err
		}
		return &Response{Status: status, Body: b}, nil
	}
	if isMap && (statusOK || hasKey(m, "body") || hasKey(m, "headers")) {
		resp := &Response{Status: 200}
		if statusOK {
			resp.Status = status
		}
		if hdrs, ok := m["headers"].(map[string]any); ok {
			resp.Headers = map[string]string{}
			for k, val := range hdrs {
				resp.Headers[k] = fmt.Sprintf("%v", val)
			}
		}
		body := m["body"]
		b, err := marshalBody(body)
		if err != nil {
			return nil, err
		}
		resp.Body = b
		return resp, nil
	}
	// Bare value -> 200 JSON body.
	b, err := marshalBody(exported)
	if err != nil {
		return nil, err
	}
	return &Response{Status: 200, Body: b}, nil
}

// marshalBody serializes a response body. A string body is passed through
// verbatim (already text - JSON string, HTML, plain text), so a handler can
// return non-JSON payloads; any other value is JSON-encoded.
func marshalBody(body any) (json.RawMessage, error) {
	if body == nil {
		return json.RawMessage("null"), nil
	}
	if s, ok := body.(string); ok {
		return json.RawMessage(s), nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("response body is not JSON-serializable: %w", err)
	}
	return b, nil
}

// httpStatus reads an envelope's status: a whole number in the HTTP status range.
func httpStatus(v any) (int, bool) {
	var n int
	switch x := v.(type) {
	case int64:
		n = int(x)
	case int:
		n = x
	case float64:
		if x != float64(int(x)) {
			return 0, false
		}
		n = int(x)
	default:
		return 0, false
	}
	return n, n >= 100 && n <= 599
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
