// Package funcexec is the single home for executing a model-authored function
// handler. Both call paths - the public HTTP endpoint and the event/cron trigger
// runner - go through Execute, so the injected ctx capabilities, their guards,
// and the declared-write policy are defined once and can't drift between the two.
package funcexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"openfabrica/internal/actionexec"
	"openfabrica/internal/funcsandbox"
	"openfabrica/internal/netguard"
	"openfabrica/internal/projectwrite"
	"openfabrica/internal/sqlguard"
	"openfabrica/security"
	"openfabrica/store"
)

const (
	queryTimeout    = 10 * time.Second
	maxQueryRows    = 1000
	maxFetchBytes   = 256 * 1024
	fetchTimeout    = 15 * time.Second
	maxSQLTextBytes = 16 * 1024
	llmTimeout      = 60 * time.Second
	defaultLLMMax   = 512
	maxLLMMax       = 4096
)

// LLMFunc runs one completion against the project's configured default provider
// and returns the text. It is what backs ctx.llm; callers build one with NewLLM.
type LLMFunc func(prompt, system string, maxTokens int) (string, error)

// Runtime carries the per-invocation capabilities the executor injects. Bundled
// in a struct (rather than a growing positional arg list) so a new capability is
// one field, not a signature change at every call site. LLM is optional - nil
// means the project has no configured provider and ctx.llm reports unavailable.
type Runtime struct {
	Reader *sql.DB
	Writer *sql.DB
	Enc    *security.Encryptor
	LLM    LLMFunc
	// ProjectID and GlobalDB back ctx.checkout: the checkout path loads the project's
	// payment provider (a project or global connector) and checks for a user-flow replay.
	ProjectID int
	GlobalDB  *sql.DB
}

// CheckoutFunc opens a hosted checkout for a handler's ctx.checkout call and returns
// the result the handler sees ({checkout_url, payment_id, external_id, amount,
// currency}). The payments tool package registers the implementation (RegisterCheckout)
// so the handler runs the very code manage_payments create_checkout runs - the payments
// row it writes is what the webhook later reconciles into payment.completed.
type CheckoutFunc func(rt Runtime, args map[string]any) (map[string]any, error)

var checkoutImpl CheckoutFunc

// RegisterCheckout installs the ctx.checkout implementation; called from the payments
// tool package's init.
func RegisterCheckout(f CheckoutFunc) { checkoutImpl = f }

// systemTableRef matches a reference to a reserved of_ table at an identifier
// boundary (so a column like number_of_items is not falsely rejected). Mirrors
// the query endpoint's guard; the security-critical verb/stacked checks come
// from the shared sqlguard package below.
var systemTableRef = regexp.MustCompile(`(?i)\bof_\w*`)

// identifierRef matches one SQL identifier, used to test a query's names
// against the project's PASSWORD columns at the same identifier boundary
// systemTableRef uses.
var identifierRef = regexp.MustCompile(`[A-Za-z_]\w*`)

// Spec is a stored function's executable definition.
type Spec struct {
	Code        string        // the handler(req, ctx) source
	SecretsJSON string        // JSON array of secret names the handler may read
	Timeout     time.Duration // per-invocation wall-clock cap (0 = funcsandbox default)
}

// Execute compiles and runs a handler against req, then applies any declared
// writes in one transaction. rt supplies the reader/writer handles, encryptor,
// and LLM invoker (nil members degrade cleanly). Returns the handler's response.
func Execute(rt Runtime, spec Spec, req map[string]any) (*funcsandbox.Response, error) {
	handler, err := funcsandbox.Prepare(spec.Code)
	if err != nil {
		return nil, err
	}
	resp, writes, err := handler.Run(req, deps(rt, spec))
	if err != nil {
		return nil, err
	}
	if err := applyWrites(rt.Reader, rt.Writer, writes); err != nil {
		return nil, err
	}
	return resp, nil
}

// deps builds the injected ctx capabilities for one invocation.
func deps(rt Runtime, spec Spec) funcsandbox.Deps {
	// Resolved on the first ctx.query and reused for the rest of the
	// invocation: a handler that never queries pays no read, and one that
	// queries up to its cap pays exactly one.
	hashedCols := sync.OnceValues(func() (map[string]bool, error) {
		return store.LoadHashColumnNames(rt.Reader)
	})
	var checkout func(map[string]any) (map[string]any, error)
	if checkoutImpl != nil {
		checkout = func(args map[string]any) (map[string]any, error) { return checkoutImpl(rt, args) }
	}
	return funcsandbox.Deps{
		NowMS:    time.Now().UnixMilli(),
		Budget:   spec.Timeout,
		LLM:      rt.LLM, // nil-safe: the sandbox throws "not available" when nil
		Checkout: checkout,

		Query: func(query string, params []any) ([]map[string]any, error) {
			// Fail closed, as the declared-write path does: without the
			// projection there is no telling whether the query touches a
			// secret. An unregistered table reports an empty set with no
			// error, so an error here is a genuine database failure.
			hashed, err := hashedCols()
			if err != nil {
				return nil, fmt.Errorf("cannot read secure-column projection: %w", err)
			}
			trimmed, rejectErr := guardReadOnlySelect(query, hashed)
			if rejectErr != "" {
				return nil, fmt.Errorf("%s", rejectErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
			defer cancel()
			rows, err := runQuery(ctx, rt.Reader, trimmed, params)
			if err != nil {
				return nil, err
			}
			stripHashColumns(rows, hashed)
			return rows, nil
		},

		Fetch: func(url string, opts funcsandbox.FetchOptions) (funcsandbox.FetchResult, error) {
			return runFetch(url, opts)
		},

		Secret: func(name string) (string, error) {
			if !secretAllowed(spec.SecretsJSON, name) {
				return "", fmt.Errorf("secret %q is not on this function's allowlist", name)
			}
			if rt.Enc == nil {
				return "", fmt.Errorf("secrets are not available")
			}
			var encVal string
			if err := rt.Reader.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", name).Scan(&encVal); err != nil {
				return "", fmt.Errorf("secret %q not found", name)
			}
			return rt.Enc.Decrypt(encVal)
		},
	}
}

// NewRuntime builds the Runtime every caller of a project's server function shares:
// the project's reader and writer, the secrets encryptor, ctx.llm on the project's
// model under the unattended budget, and the global DB ctx.checkout needs.
func NewRuntime(projectID int, reader, writer *sql.DB, llmDeps actionexec.CompleteDeps) Runtime {
	return Runtime{
		Reader:    reader,
		Writer:    writer,
		Enc:       llmDeps.Encryptor,
		LLM:       NewLLM(projectID, writer, llmDeps),
		ProjectID: projectID,
		GlobalDB:  llmDeps.GlobalDB,
	}
}

// NewLLM builds the ctx.llm invoker: one completion through actionexec.Complete, the
// path a trigger's llm_call and a job's llm_prompt take too (project model, daily
// budget, of_llm_calls row under source "function", retry policy). Returns nil when no
// global DB is available.
func NewLLM(projectID int, projectDB *sql.DB, d actionexec.CompleteDeps) LLMFunc {
	if d.GlobalDB == nil {
		return nil
	}
	return func(prompt, system string, maxTokens int) (string, error) {
		if maxTokens <= 0 {
			maxTokens = defaultLLMMax
		}
		if maxTokens > maxLLMMax {
			maxTokens = maxLLMMax
		}
		out, err := actionexec.Complete(context.Background(), d, projectID, projectDB, "function", actionexec.CompleteRequest{
			Prompt: prompt, System: system, MaxTokens: maxTokens, Timeout: llmTimeout,
		})
		if err != nil {
			return "", fmt.Errorf("ctx.llm: %w", err)
		}
		return out, nil
	}
}

// QueryReadOnly runs one guarded read-only SELECT for callers OUTSIDE the
// sandbox (the agent runtime's data-query tool), applying the exact same guard,
// timeout, and PASSWORD-column stripping that ctx.query gives a function handler.
// Routing both through this one path keeps the read surface from drifting into a
// weaker variant. The returned error's message is safe to surface: it is either
// a guard rejection telling the caller how to fix the SQL, or a database error.
func QueryReadOnly(reader *sql.DB, query string, params []any) ([]map[string]any, error) {
	hashed, err := store.LoadHashColumnNames(reader)
	if err != nil {
		return nil, fmt.Errorf("cannot read secure-column projection: %w", err)
	}
	trimmed, rejectErr := guardReadOnlySelect(query, hashed)
	if rejectErr != "" {
		return nil, fmt.Errorf("%s", rejectErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := runQuery(ctx, reader, trimmed, params)
	if err != nil {
		return nil, err
	}
	stripHashColumns(rows, hashed)
	return rows, nil
}

// guardReadOnlySelect enforces "single SELECT, no system tables, no PASSWORD
// columns" on handler SQL, delegating verb/stacked-statement checks to the shared
// sqlguard so this surface can't drift into a weaker variant. Returns the trimmed
// query and a non-empty message when rejected.
func guardReadOnlySelect(query string, hashed map[string]bool) (string, string) {
	if len(query) > maxSQLTextBytes {
		return "", "sql is too long"
	}
	trimmed := strings.TrimRight(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";")), " \t\r\n")
	if trimmed == "" {
		return "", "sql is required"
	}
	// EffectiveVerb resolves a leading `WITH` to the verb after its CTE definitions,
	// so a read-only CTE is accepted here on the same terms as at build time and a
	// WITH-wrapped write is still refused.
	switch sqlguard.EffectiveVerb(trimmed) {
	case "SELECT", "VALUES":
	default:
		return "", "ctx.query runs read-only statements only: SELECT, VALUES, or a WITH ... SELECT"
	}
	if sqlguard.HasStackedStatements(trimmed) {
		return "", "multiple SQL statements are not allowed"
	}
	if m := systemTableRef.FindString(trimmed); m != "" {
		return "", fmt.Sprintf("cannot reference system table: %s", strings.ToLower(m))
	}
	if m := hashColumnRef(trimmed, hashed); m != "" {
		return "", fmt.Sprintf("cannot reference PASSWORD column: %s", m)
	}
	return trimmed, ""
}

// hashColumnRef returns the first PASSWORD column the query names, or "". Naming
// one is refused rather than filtered: a hash the query can address is a hash it
// can test (aliases and WHERE comparisons defeat result-side checks).
// Over-refusing costs a handler one rewrite; under-refusing leaks a credential.
func hashColumnRef(query string, hashed map[string]bool) string {
	if len(hashed) == 0 {
		return ""
	}
	for _, ident := range identifierRef.FindAllString(query, -1) {
		if hashed[strings.ToLower(ident)] {
			return strings.ToLower(ident)
		}
	}
	return ""
}

// stripHashColumns drops PASSWORD columns from query results. It covers the
// wildcard the reference guard cannot see: SELECT * names no column yet returns
// every one of them.
func stripHashColumns(rows []map[string]any, hashed map[string]bool) {
	if len(hashed) == 0 {
		return
	}
	for _, row := range rows {
		for col := range row {
			if hashed[strings.ToLower(col)] {
				delete(row, col)
			}
		}
	}
}

// runQuery executes a validated SELECT and returns up to maxQueryRows rows as
// column->value maps ([]byte rendered as string so the handler sees TEXT, not
// base64).
func runQuery(ctx context.Context, reader *sql.DB, query string, params []any) ([]map[string]any, error) {
	rows, err := reader.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		if len(out) >= maxQueryRows {
			break
		}
		holders := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := holders[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = holders[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Fetch performs one SSRF-guarded outbound request for callers OUTSIDE the
// sandbox (the agent runtime's http_fetch tool), through the exact same
// validator, pinned-resolution client, and response cap that back ctx.fetch.
// Routing both through runFetch keeps the outbound surface from drifting.
func Fetch(url string, opts funcsandbox.FetchOptions) (funcsandbox.FetchResult, error) {
	return runFetch(url, opts)
}

// runFetch performs one SSRF-guarded outbound request for ctx.fetch, reusing the
// same validator + pinned-resolution client manage_http and the trigger runner
// use, and bounding the response read.
func runFetch(url string, opts funcsandbox.FetchOptions) (funcsandbox.FetchResult, error) {
	if err := netguard.ValidateExternalURL(url); err != nil {
		return funcsandbox.FetchResult{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	var body io.Reader
	if opts.Body != "" {
		body = strings.NewReader(opts.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return funcsandbox.FetchResult{}, err
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}
	resp, err := netguard.SSRFSafeClient().Do(req)
	if err != nil {
		return funcsandbox.FetchResult{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	headers := map[string]string{}
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return funcsandbox.FetchResult{Status: resp.StatusCode, Headers: headers, Body: string(respBody)}, nil
}

// applyWrites runs the handler's declared writes in ONE transaction, refusing
// of_* system tables, PASSWORD/ENCRYPTED columns, and any table whose
// secure-column projection cannot be read. Fail-closed twice over: an unreadable
// projection refuses rather than assumes, and one refused write fails the whole batch.
func applyWrites(reader, writer *sql.DB, writes []funcsandbox.Write) error {
	if len(writes) == 0 {
		return nil
	}
	for _, w := range writes {
		if _, err := store.SanitizedTableName(w.Table); err != nil {
			return fmt.Errorf("write to %q rejected: %w", w.Table, err)
		}
		secure, err := store.LoadSecureColumns(reader, w.Table)
		if err != nil {
			// Can't read the projection, so we can't tell whether this write
			// targets a secret. Refuse rather than assume it doesn't. (An
			// unregistered table is not an error - it reports an empty projection.)
			return fmt.Errorf("write to %q rejected: cannot read secure-column projection: %w", w.Table, err)
		}
		if writeTouchesSecure(w, secure) {
			return fmt.Errorf("write to %q rejected: targets a PASSWORD/ENCRYPTED column", w.Table)
		}
	}
	tx, err := writer.Begin()
	if err != nil {
		return err
	}
	for _, w := range writes {
		var e error
		switch w.Op {
		case "insert":
			_, e = projectwrite.Insert(tx, w.Table, w.Row)
		case "update":
			_, e = projectwrite.Update(tx, w.Table, w.Row, w.Where)
		case "upsert":
			_, e = projectwrite.Upsert(tx, w.Table, w.Row, w.Conflict)
		default:
			e = fmt.Errorf("unknown write op %q", w.Op)
		}
		if e != nil {
			_ = tx.Rollback()
			return e
		}
	}
	return tx.Commit()
}

// secretAllowed reports whether name is on the JSON allowlist. Empty = none.
func secretAllowed(allowlistJSON, name string) bool {
	if strings.TrimSpace(allowlistJSON) == "" {
		return false
	}
	var names []string
	if json.Unmarshal([]byte(allowlistJSON), &names) != nil {
		return false
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func writeTouchesSecure(w funcsandbox.Write, secure map[string]string) bool {
	for col := range w.Row {
		if _, ok := secure[col]; ok {
			return true
		}
	}
	return false
}
