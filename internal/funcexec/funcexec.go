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
	"strings"
	"sync"
	"time"

	"openfabrica/events"
	"openfabrica/internal/actionexec"
	"openfabrica/internal/folderwatch"
	"openfabrica/internal/funcsandbox"
	"openfabrica/internal/lan"
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

// LLMCtxFunc is the context-aware variant of LLMFunc so in-flight LLM calls
// cancel when the handler's wall-clock budget expires or the HTTP client disconnects.
type LLMCtxFunc func(ctx context.Context, prompt, system string, maxTokens int) (string, error)

// Runtime carries the per-invocation capabilities the executor injects. Bundled
// in a struct (rather than a growing positional arg list) so a new capability is
// one field, not a signature change at every call site. LLM is optional - nil
// means the project has no configured provider and ctx.llm reports unavailable.
type Runtime struct {
	Reader *sql.DB
	Writer *sql.DB
	Enc    *security.Encryptor
	LLM    LLMFunc
	LLMCtx LLMCtxFunc
	// ProjectID and GlobalDB back ctx.checkout: the checkout path loads the project's
	// payment provider (a project or global connector) and checks for a user-flow replay.
	ProjectID int
	GlobalDB  *sql.DB
	Bus       *events.Bus
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

// MQTTPublishFunc publishes a message to a project's MQTT broker from ctx.mqtt.publish.
type MQTTPublishFunc func(projectID int, topic string, payload []byte, qos byte, retain bool) error

// MQTTStatsFunc returns live broker stats (connected_clients, clients, subscriptions) for a project.
type MQTTStatsFunc func(projectID int) (map[string]any, bool)

var (
	mqttPublishImpl MQTTPublishFunc
	mqttStatsImpl   MQTTStatsFunc
)

// RegisterMQTTPublish installs the ctx.mqtt.publish implementation at server wiring time.
func RegisterMQTTPublish(f MQTTPublishFunc) { mqttPublishImpl = f }

// RegisterMQTTStats installs the live MQTT broker stats hook at server wiring time.
func RegisterMQTTStats(f MQTTStatsFunc) { mqttStatsImpl = f }

// PublishMQTT publishes to a project's live MQTT broker when wired (no-op when nil).
func PublishMQTT(projectID int, topic string, payload []byte, qos byte, retain bool) error {
	if mqttPublishImpl == nil {
		return nil
	}
	return mqttPublishImpl(projectID, topic, payload, qos, retain)
}

// MQTTStats returns live broker stats for projectID when wired and active.
func MQTTStats(projectID int) (map[string]any, bool) {
	if mqttStatsImpl == nil {
		return nil, false
	}
	return mqttStatsImpl(projectID)
}

// Spec is a stored function's executable definition.
type Spec struct {
	Code        string        // the handler(req, ctx) source
	SecretsJSON string        // JSON array of secret names the handler may read
	Timeout     time.Duration // per-invocation wall-clock cap (0 = funcsandbox default)
}

// Execute compiles and runs a handler against req, then applies any declared
// writes in one transaction. Prefer ExecuteContext when a request context exists.
func Execute(rt Runtime, spec Spec, req map[string]any) (*funcsandbox.Response, error) {
	return ExecuteContext(context.Background(), rt, spec, req)
}

// ExecuteContext compiles and runs a handler under parent ctx bounded by spec.Timeout,
// cancelling both JS execution and in-flight host I/O (ctx.query, ctx.fetch, ctx.llm)
// when either the budget expires or the caller disconnects.
func ExecuteContext(parent context.Context, rt Runtime, spec Spec, req map[string]any) (*funcsandbox.Response, error) {
	if parent == nil {
		parent = context.Background()
	}
	budget := spec.Timeout
	if budget <= 0 {
		budget = funcsandbox.Budget
	}
	runCtx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	handler, err := funcsandbox.Prepare(spec.Code)
	if err != nil {
		return nil, err
	}
	resp, writes, err := handler.Run(req, deps(runCtx, rt, spec))
	if err != nil {
		return nil, err
	}
	if err := applyWrites(rt, writes); err != nil {
		return nil, err
	}
	return resp, nil
}

func runWatchList(reader *sql.DB, target string) (map[string]any, error) {
	if reader == nil {
		return map[string]any{"ok": false, "available": false, "error": "project database is unavailable"}, nil
	}
	watchers, err := store.ListFolderWatchers(reader)
	if err != nil || len(watchers) == 0 {
		return map[string]any{"ok": false, "available": false, "error": "no folder watcher is configured"}, nil
	}
	t := strings.TrimSpace(target)
	var chosen *store.FolderWatcher
	for i := range watchers {
		w := &watchers[i]
		if !w.Enabled {
			continue
		}
		if t == "" || strings.EqualFold(w.Name, t) || strings.EqualFold(w.Folder, t) {
			chosen = w
			break
		}
	}
	if chosen == nil {
		// Fall back to the first enabled watcher so callers passing a config key or relative path still resolve safely to the project's registered watch folder.
		for i := range watchers {
			if watchers[i].Enabled {
				chosen = &watchers[i]
				break
			}
		}
	}
	if chosen == nil {
		return map[string]any{"ok": false, "available": false, "error": "no enabled folder watcher found"}, nil
	}
	entries, available, lerr := folderwatch.ListWatchDir(chosen.Folder, chosen.Glob)
	if lerr != nil || !available {
		errMsg := "Watching is unavailable - the directory could not be read"
		if lerr != nil {
			errMsg = lerr.Error()
		}
		return map[string]any{
			"ok":        false,
			"available": false,
			"watcher":   chosen.Name,
			"folder":    chosen.Folder,
			"path":      chosen.Folder,
			"error":     errMsg,
			"files":     []any{},
			"entries":   []any{},
		}, nil
	}
	items := make([]map[string]any, len(entries))
	for i, e := range entries {
		items[i] = map[string]any{
			"path":       e.Path,
			"name":       e.Name,
			"ext":        e.Ext,
			"size_bytes": e.SizeBytes,
			"size":       e.Size,
			"mtime_ms":   e.ModTimeMS,
			"mtime":      e.ModTime,
			"is_dir":     e.IsDir,
		}
	}
	return map[string]any{
		"ok":        true,
		"available": true,
		"watcher":   chosen.Name,
		"folder":    chosen.Folder,
		"path":      chosen.Folder,
		"glob":      chosen.Glob,
		"poll_sec":  chosen.PollSec,
		"count":     len(items),
		"files":     items,
		"entries":   items,
	}, nil
}

// deps builds the injected ctx capabilities for one invocation.
func deps(runCtx context.Context, rt Runtime, spec Spec) funcsandbox.Deps {
	// Resolved on the first ctx.query and reused for the rest of the
	// invocation: a handler that never queries pays no read, and one that
	// queries up to its cap pays exactly one.
	type secureProjection struct {
		cols   map[string]bool
		tables map[string]bool
	}
	secureProj := sync.OnceValues(func() (secureProjection, error) {
		cols, err := store.LoadSecureColumnNames(rt.Reader)
		if err != nil {
			return secureProjection{}, err
		}
		tables, err := store.LoadSecureTableNames(rt.Reader)
		if err != nil {
			return secureProjection{}, err
		}
		return secureProjection{cols: cols, tables: tables}, nil
	})
	var checkout func(map[string]any) (map[string]any, error)
	if checkoutImpl != nil {
		checkout = func(args map[string]any) (map[string]any, error) { return checkoutImpl(rt, args) }
	}
	var mqttPub func(topic, payload string, qos int, retain bool) error
	if mqttPublishImpl != nil {
		mqttPub = func(topic, payload string, qos int, retain bool) error {
			if qos < 0 {
				qos = 0
			} else if qos > 1 {
				qos = 1
			}
			return mqttPublishImpl(rt.ProjectID, topic, []byte(payload), byte(qos), retain)
		}
	}
	llmFn := rt.LLM
	if rt.LLMCtx != nil {
		llmFn = func(prompt, system string, maxTokens int) (string, error) {
			return rt.LLMCtx(runCtx, prompt, system, maxTokens)
		}
	}
	return funcsandbox.Deps{
		Ctx:         runCtx,
		NowMS:       time.Now().UnixMilli(),
		Budget:      spec.Timeout,
		LLM:         llmFn, // nil-safe: the sandbox throws "not available" when nil
		Checkout:    checkout,
		MQTTPublish: mqttPub,
		LANWake: func(mac, broadcast string, port int) (string, int, error) {
			return lan.Wake(mac, broadcast, port)
		},
		LANPing: func(host string, port int, timeoutMS int) (bool, int64, error) {
			if timeoutMS <= 0 {
				timeoutMS = 1000
			}
			if timeoutMS > 2000 {
				timeoutMS = 2000
			}
			return lan.TCPPing(runCtx, host, port, time.Duration(timeoutMS)*time.Millisecond)
		},
		WatchList: func(target string) (map[string]any, error) {
			return runWatchList(rt.Reader, target)
		},

		Query: func(query string, params []any) ([]map[string]any, error) {
			// Fail closed, as the declared-write path does: without the
			// projection there is no telling whether the query touches a
			// secret. An unregistered table reports an empty set with no
			// error, so an error here is a genuine database failure.
			proj, err := secureProj()
			if err != nil {
				return nil, fmt.Errorf("cannot read secure-column projection: %w", err)
			}
			trimmed, rejectErr := guardReadOnlySelect(query, proj.cols, proj.tables)
			if rejectErr != "" {
				return nil, fmt.Errorf("%s", rejectErr)
			}
			ctx, cancel := context.WithTimeout(runCtx, queryTimeout)
			defer cancel()
			rows, err := runQuery(ctx, rt.Reader, trimmed, params)
			if err != nil {
				return nil, err
			}
			stripHashColumns(rows, proj.cols)
			return rows, nil
		},

		Fetch: func(url string, opts funcsandbox.FetchOptions) (funcsandbox.FetchResult, error) {
			return runFetch(runCtx, url, opts)
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
	llmCtx := NewLLMCtx(projectID, writer, llmDeps)
	var llmFn LLMFunc
	if llmCtx != nil {
		llmFn = func(prompt, system string, maxTokens int) (string, error) {
			return llmCtx(context.Background(), prompt, system, maxTokens)
		}
	}
	return Runtime{
		Reader:    reader,
		Writer:    writer,
		Enc:       llmDeps.Encryptor,
		LLM:       llmFn,
		LLMCtx:    llmCtx,
		ProjectID: projectID,
		GlobalDB:  llmDeps.GlobalDB,
	}
}

// NewLLM builds the ctx.llm invoker: one completion through actionexec.Complete, the
// path a trigger's llm_call and a job's llm_prompt take too (project model, daily
// budget, of_llm_calls row under source "function", retry policy). Returns nil when no
// global DB is available.
func NewLLM(projectID int, projectDB *sql.DB, d actionexec.CompleteDeps) LLMFunc {
	llmCtx := NewLLMCtx(projectID, projectDB, d)
	if llmCtx == nil {
		return nil
	}
	return func(prompt, system string, maxTokens int) (string, error) {
		return llmCtx(context.Background(), prompt, system, maxTokens)
	}
}

// NewLLMCtx builds a context-aware ctx.llm invoker bound to the handler's run context.
func NewLLMCtx(projectID int, projectDB *sql.DB, d actionexec.CompleteDeps) LLMCtxFunc {
	if d.GlobalDB == nil {
		return nil
	}
	return func(ctx context.Context, prompt, system string, maxTokens int) (string, error) {
		if maxTokens <= 0 {
			maxTokens = defaultLLMMax
		}
		if maxTokens > maxLLMMax {
			maxTokens = maxLLMMax
		}
		out, err := actionexec.Complete(ctx, d, projectID, projectDB, "function", actionexec.CompleteRequest{
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
// timeout, and secure-column (PASSWORD/ENCRYPTED) stripping that ctx.query gives
// a function handler. Routing both through this one path keeps the read surface
// from drifting into a weaker variant.
func QueryReadOnly(reader *sql.DB, query string, params []any) ([]map[string]any, error) {
	secretCols, err := store.LoadSecureColumnNames(reader)
	if err != nil {
		return nil, fmt.Errorf("cannot read secure-column projection: %w", err)
	}
	secretTables, err := store.LoadSecureTableNames(reader)
	if err != nil {
		return nil, fmt.Errorf("cannot read secure-table projection: %w", err)
	}
	trimmed, rejectErr := guardReadOnlySelect(query, secretCols, secretTables)
	if rejectErr != "" {
		return nil, fmt.Errorf("%s", rejectErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := runQuery(ctx, reader, trimmed, params)
	if err != nil {
		return nil, err
	}
	stripHashColumns(rows, secretCols)
	return rows, nil
}

// guardReadOnlySelect enforces "single SELECT, no system/engine tables, no memory-hog
// functions, no PASSWORD/ENCRYPTED columns, no compound queries over tables with
// secure columns" on handler and agent SQL, delegating to the shared sqlguard.
func guardReadOnlySelect(query string, secretCols, secretTables map[string]bool) (string, string) {
	if len(query) > maxSQLTextBytes {
		return "", "sql is too long"
	}
	trimmed, rejectErr := sqlguard.CheckReadOnlySelect(query)
	if rejectErr != "" {
		return "", rejectErr
	}
	if rejectErr := sqlguard.CheckQueryTargets(trimmed, nil, secretCols, secretTables); rejectErr != "" {
		return "", rejectErr
	}
	return trimmed, ""
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
	return runFetch(context.Background(), url, opts)
}

// runFetch performs one SSRF-guarded outbound request for ctx.fetch, reusing the
// same validator + pinned-resolution client manage_http and the trigger runner
// use, and bounding the response read.
func runFetch(parent context.Context, url string, opts funcsandbox.FetchOptions) (funcsandbox.FetchResult, error) {
	if err := netguard.ValidateExternalURL(url); err != nil {
		return funcsandbox.FetchResult{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(parent, fetchTimeout)
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
func applyWrites(rt Runtime, writes []funcsandbox.Write) error {
	if len(writes) == 0 {
		return nil
	}
	reader, writer := rt.Reader, rt.Writer
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
	type insertedEvent struct {
		table string
		id    int64
		row   map[string]any
	}
	var inserts []insertedEvent
	for _, w := range writes {
		var res sql.Result
		var e error
		switch w.Op {
		case "insert":
			res, e = projectwrite.Insert(tx, w.Table, w.Row)
			if e == nil && res != nil {
				id, _ := res.LastInsertId()
				inserts = append(inserts, insertedEvent{table: w.Table, id: id, row: w.Row})
			}
		case "update":
			_, e = projectwrite.Update(tx, w.Table, w.Row, w.Where)
		case "upsert":
			_, e = projectwrite.Upsert(tx, w.Table, w.Row, w.Conflict)
		case "delete":
			_, e = projectwrite.Delete(tx, w.Table, w.Where)
		default:
			e = fmt.Errorf("unknown write op %q", w.Op)
		}
		if e != nil {
			_ = tx.Rollback()
			return e
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if rt.Bus != nil && rt.ProjectID > 0 && len(inserts) > 0 {
		for _, ins := range inserts {
			publishDeclaredInsert(rt, ins.table, ins.id, ins.row)
		}
	}
	return nil
}

// publishDeclaredInsert emits a data.insert event after a server function's
// ctx.insert commits, resolving the physical table's primary REST endpoint (when
// one exists) so SSE streams over that table (manage_sse) receive the new row live.
func publishDeclaredInsert(rt Runtime, table string, id int64, row map[string]any) {
	physTable, err := store.SanitizedTableName(table)
	if err != nil {
		return
	}
	payload := make(map[string]any, len(row)+3)
	for k, v := range row {
		payload[k] = v
	}
	if reader := rt.Reader; reader != nil && id > 0 {
		if rows, rerr := store.GetDynamicRow(context.Background(), reader, store.DynamicRowLookup{
			Table:      physTable,
			QuotedCols: "*",
			LookupCol:  "id",
			LookupVal:  id,
		}); rerr == nil && len(rows) > 0 {
			for k, v := range rows[0] {
				payload[k] = v
			}
		}
		var epPath string
		_ = reader.QueryRow(`SELECT path FROM of_api_endpoints WHERE LOWER(table_name) = LOWER(?) ORDER BY id LIMIT 1`, physTable).Scan(&epPath)
		if epPath != "" {
			payload["endpoint"] = epPath
		}
	}
	payload["table"] = physTable
	if id > 0 {
		payload["id"] = id
	}
	rt.Bus.Publish(events.NewEvent(events.EventDataInsert, rt.ProjectID, payload))
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
	if len(secure) == 0 {
		return false
	}
	secLower := make(map[string]bool, len(secure))
	for col := range secure {
		secLower[strings.ToLower(col)] = true
	}
	for col := range w.Row {
		if secLower[strings.ToLower(col)] {
			return true
		}
	}
	for col := range w.Where {
		if secLower[strings.ToLower(col)] {
			return true
		}
	}
	for _, col := range w.Conflict {
		if secLower[strings.ToLower(col)] {
			return true
		}
	}
	return false
}
