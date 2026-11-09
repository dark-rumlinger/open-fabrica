package triggers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/actionexec"
	"openfabrica/internal/netguard"
	"openfabrica/internal/projectwrite"
	"openfabrica/internal/safego"
	"openfabrica/internal/sqlguard"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/tools"
	"openfabrica/webhooks"
)

// maxConcurrentActions caps in-flight trigger actions across all projects.
// Without a ceiling a single hot event with many subscribed triggers fans out
// into unbounded goroutines (some of them slow LLM or HTTP calls) with no
// backpressure. Mirrors the webhook dispatcher's delivery cap.
const maxConcurrentActions = 16

// toolActionTimeout bounds a single tool-backed action (send_email/push).
// These do synchronous provider HTTP; without a deadline a hung provider would
// hold one of the maxConcurrentActions slots indefinitely.
const toolActionTimeout = 60 * time.Second

// Runner executes server-side actions when subscribed events fire.
// Similar to the webhook Dispatcher but runs actions locally (send email,
// HTTP request, insert/update data) instead of calling external URLs.
type Runner struct {
	projectDBMgr *db.ProjectDBManager
	bus          *events.Bus
	encryptor    *security.Encryptor
	broadcaster  events.WSBroadcaster // optional, nil if WS hub not available yet
	logger       *slog.Logger
	client       *http.Client
	// ctx is the runner's lifecycle context: outbound action HTTP must abort
	// when it is canceled at shutdown. Nil on a zero-value Runner (tests).
	ctx         context.Context
	llmRegistry *llm.Set        // optional, set via SetLLMRegistry; required for llm_call / ai_embed
	globalDB    *sql.DB         // global DB (provider rows live here); set via SetGlobalDB
	toolExec    *tools.Executor // optional, set via SetTools; backs the messaging actions
	toolEnv     *tools.Env      // optional, set via SetTools
	sem         chan struct{}   // bounds concurrent action execution
	// allowPrivateHosts disables the SSRF guard on outbound http_request /
	// trigger_webhook actions. Off by default so triggers cannot reach the
	// loopback admin API, link-local metadata endpoints, or the internal
	// network; only enabled where reaching internal services is intended.
	allowPrivateHosts bool
	// budgetFn returns the current per-project daily token budget (0 = unlimited),
	// set via SetBudgetFn. llm_call gates on it against the shared unattended-spend
	// tally so an event-driven completion cannot bill past the daily limit.
	budgetFn func() int
	// callLLM runs llm_call completions under the engine's retry policy; set via
	// SetLLMCaller. Nil calls the provider directly.
	callLLM llm.CallFunc
	// llmHTTPClient is the instance's shared LLM client; set via SetLLMHTTPClient.
	llmHTTPClient *http.Client
	// webhooks delivers trigger_webhook actions through the outgoing-webhook
	// sender (retries, signing, delivery log); set via SetWebhookSender.
	webhooks *webhooks.Dispatcher
	// embedFn substitutes the embeddings provider in tests; nil uses llm.EmbedTexts.
	embedFn func(ctx context.Context, texts []string) ([][]float32, error)
}

// NewRunner creates an action runner and subscribes to all events. ctx is the
// process-lifecycle context; canceling it aborts in-flight outbound HTTP.
func NewRunner(projectDBMgr *db.ProjectDBManager, bus *events.Bus, encryptor *security.Encryptor, ctx context.Context) *Runner {
	r := &Runner{
		projectDBMgr: projectDBMgr,
		bus:          bus,
		encryptor:    encryptor,
		ctx:          ctx,
		logger:       slog.With("component", "action_runner"),
		// SSRF-hardened: trigger HTTP/webhook actions POST to config-supplied
		// (often event-templated) URLs, so the client re-validates redirects and
		// blocks private/internal IPs at dial time. URLs are also pre-validated
		// with netguard.ValidateExternalURL before each request.
		client: netguard.SSRFSafeClient(),
		sem:    make(chan struct{}, maxConcurrentActions),
	}

	bus.SubscribeAll(r.handleEvent)

	return r
}

// lifecycle is the runner's process-lifecycle context, canceled first at
// shutdown, so every action's outbound work stops with it. A zero-value Runner
// (tests) has none and runs under Background.
func (r *Runner) lifecycle() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// SetBroadcaster sets the WebSocket broadcaster for ws_broadcast actions.
// Called after the public server creates the WS hub.
func (r *Runner) SetBroadcaster(b events.WSBroadcaster) {
	r.broadcaster = b
}

// SetLLMRegistry wires the llm.Set instance registry so llm_call and
// ai_embed actions can resolve a provider at fire time. Optional; if unset
// those action types log a warning and skip.
func (r *Runner) SetLLMRegistry(reg *llm.Set, globalDB *sql.DB) {
	r.llmRegistry = reg
	r.globalDB = globalDB
}

// SetBudgetFn wires the per-project daily token budget resolver so llm_call
// respects the same daily AI spend limit as the engine. Optional; if unset,
// llm_call runs without a budget gate (unbounded, the prior behavior).
func (r *Runner) SetBudgetFn(fn func() int) {
	r.budgetFn = fn
}

// SetWebhookSender wires the outgoing-webhook dispatcher trigger_webhook sends through.
func (r *Runner) SetWebhookSender(d *webhooks.Dispatcher) {
	r.webhooks = d
}

// SetLLMHTTPClient wires the instance's shared LLM HTTP client, so llm_call and a
// run_function's ctx.llm honour the configured LLM timeout and connection pool.
func (r *Runner) SetLLMHTTPClient(hc *http.Client) {
	r.llmHTTPClient = hc
}

// completeDeps is what llm_call and a run_function's ctx.llm need
// (actionexec.Complete).
func (r *Runner) completeDeps() actionexec.CompleteDeps {
	return actionexec.CompleteDeps{
		GlobalDB:   r.globalDB,
		Encryptor:  r.encryptor,
		HTTPClient: r.llmHTTPClient,
		BudgetFn:   r.budgetFn,
		Call:       r.callLLM,
	}
}

// SetLLMCaller wires the engine's call policy (engine.Engine.CallLLM) so an
// llm_call action rides out a transient failure or a 429 instead of failing.
// Optional; if unset the provider is called directly.
func (r *Runner) SetLLMCaller(call llm.CallFunc) {
	r.callLLM = call
}

// SetTools wires the shared tool executor and env so the messaging actions
// (send_email/send_push) dispatch through their tools instead of
// reimplementing provider lookup, request shaping, and auth injection.
// Calls run headless (see tools.Env.Headless). Optional; if unset those
// actions return an error rather than running.
func (r *Runner) SetTools(exec *tools.Executor, env *tools.Env) {
	r.toolExec = exec
	r.toolEnv = env.Headless()
}

// runTool executes a registered tool headlessly against the project's DB. It
// is the single seam through which event-driven actions reuse the same tool
// implementations the engine and job worker call, through the same executor
// (deadline, panic recovery, events) and ToolContext shape, so auth and request
// shaping never drift across the call paths. Returns the tool's failure message
// as an error so the trigger logger records it.
func (r *Runner) runTool(projectID int, toolName string, args map[string]any) error {
	if r.toolExec == nil || r.toolEnv == nil {
		return fmt.Errorf("%s: tool executor not wired into action runner", toolName)
	}
	pdb := r.projectDBMgr.Get(projectID)
	if pdb == nil {
		return fmt.Errorf("%s: project %d database not open", toolName, projectID)
	}
	// Bound tool execution: a messaging tool does synchronous HTTP, so without a
	// deadline a hung provider would pin one of the limited action slots forever.
	// Invoke also applies the tool's own deadline; the tighter one wins.
	ctx, cancel := context.WithTimeout(r.lifecycle(), toolActionTimeout)
	defer cancel()
	res, _, err := r.toolExec.Invoke(ctx, r.toolEnv, tools.Call{
		Name:      toolName,
		Args:      args,
		ProjectID: projectID,
		Project:   pdb,
		Logger:    r.logger,
	})
	if err != nil {
		return err
	}
	if res != nil && !res.Success {
		return fmt.Errorf("%s", res.Error)
	}
	return nil
}

// handleEvent checks if any actions are subscribed to this event type.
func (r *Runner) handleEvent(event events.Event) {
	if event.ProjectID == 0 {
		return
	}

	projectDB := r.projectDBMgr.Get(event.ProjectID)
	if projectDB == nil {
		return
	}

	// Do the trigger lookup AND fan-out in a single detached goroutine so the
	// event bus (which calls subscribers synchronously, in the publisher's
	// goroutine) is never blocked on DB I/O. The goroutine loads the matching
	// triggers, then drains them through the semaphore, so in-flight actions
	// stay globally capped at maxConcurrentActions without applying
	// backpressure to Publish.
	writer := projectDB.Writer()
	safego.Go("trigger fan-out", func() {
		type trigger struct {
			id            int
			name          string
			eventFilter   string
			triggerType   string
			triggerConfig string
		}

		var matched []trigger
		if rows, err := projectDB.Query(
			`SELECT id, name, event_filter, trigger_type, trigger_config
			 FROM of_triggers
			 WHERE event_type = ? AND is_enabled = 1`,
			string(event.Type),
		); err == nil {
			for rows.Next() {
				var t trigger
				var filter sql.NullString
				if err := rows.Scan(&t.id, &t.name, &filter, &t.triggerType, &t.triggerConfig); err != nil {
					continue
				}
				if filter.Valid {
					t.eventFilter = filter.String
				}
				matched = append(matched, t)
			}
			_ = rows.Err() // best-effort: fire whatever scanned cleanly, like a failed lookup
			// Release the read connection before the possibly-blocking fan-out.
			rows.Close()
		}

		for _, t := range matched {
			// Check event filter if present.
			if t.eventFilter != "" {
				if !matchesFilter(t.eventFilter, event.Payload) {
					continue
				}
			}
			r.sem <- struct{}{}
			go func(t trigger) {
				defer func() { <-r.sem }()
				r.execute(writer, event, t.id, t.name, t.triggerType, t.triggerConfig)
			}(t)
		}
	})
}

// matchesFilter checks if the event payload matches the filter criteria.
// Filter is a JSON object like {"table":"users"}; all keys must match.
func matchesFilter(filterJSON string, payload map[string]any) bool {
	var filter map[string]any
	if err := json.Unmarshal([]byte(filterJSON), &filter); err != nil {
		return false
	}
	for key, expected := range filter {
		actual, ok := payload[key]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", expected) {
			return false
		}
	}
	return true
}

// validIdentifier aliases security.IdentifierRegex so action SQL that builds its
// own statements (llm_call/ai_embed persistence in runner_extras.go) validates
// identifiers against the same definition as the rest of the codebase. Simple
// row insert/update goes through internal/projectwrite instead.
var validIdentifier = security.IdentifierRegex

// templateVarRe matches {{variable_name}} placeholders.
var templateVarRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// resolveTemplate replaces {{field}} placeholders with values from the event payload.
func resolveTemplate(template string, payload map[string]any) string {
	return templateVarRe.ReplaceAllStringFunc(template, func(match string) string {
		key := match[2 : len(match)-2] // strip {{ and }}
		if val, ok := payload[key]; ok {
			return fmt.Sprintf("%v", val)
		}
		return match // leave unresolved placeholders as-is
	})
}

// resolveConfigTemplates walks a parsed action config and replaces {{key}}
// placeholders in string leaves with values from the event payload.
// Substitution happens AFTER JSON parsing so a payload value containing a
// quote or comma cannot inject structural JSON (extra keys, flipped flags).
// Maps and slices are recursed; non-string scalars are left as-is.
func resolveConfigTemplates(v any, payload map[string]any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = resolveConfigTemplates(val, payload)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = resolveConfigTemplates(val, payload)
		}
		return x
	case string:
		return resolveTemplate(x, payload)
	default:
		return v
	}
}

// execute runs a single trigger based on its type.
func (r *Runner) execute(projectDB *sql.DB, event events.Event, triggerID int, name, triggerType, configJSON string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("trigger panic", "trigger", name, "panic", rec)
		}
	}()

	var config map[string]any
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		r.logger.Error("trigger: invalid config JSON", "trigger", name, "error", err)
		return
	}

	// run_sql is the one action whose primary field is raw SQL. Event payload
	// must never be substituted into the SQL text - that is an injection vector
	// (a payload value could close a literal and append its own statement). Pull
	// the raw sql aside so the generic resolver leaves it untouched; only the
	// bound `params` carry templated values, and those reach the DB as bound
	// arguments, not as SQL text. See executeRunSQL.
	var rawRunSQL any
	hadRunSQL := false
	if triggerType == "run_sql" {
		if s, ok := config["sql"]; ok {
			rawRunSQL = s
			hadRunSQL = true
			delete(config, "sql")
		}
	}
	if triggerType == "send_email" {
		if err := resolveEmailConfig(config, event.Payload); err != nil {
			r.logger.Error("trigger failed", "trigger", name, "type", triggerType, "error", err)
			return
		}
	} else {
		resolveConfigTemplates(config, event.Payload)
	}
	if hadRunSQL {
		config["sql"] = rawRunSQL
	}

	handler, ok := actionHandlers[triggerType]
	if !ok {
		r.logger.Warn("trigger: unknown trigger_type", "trigger", name, "type", triggerType)
		return
	}
	if err := handler(r, projectDB, event, config); err != nil {
		r.logger.Error("trigger failed", "trigger", name, "type", triggerType, "error", err)
		return
	}
	r.logger.Info("trigger executed", "trigger", name, "type", triggerType)
}

// actionHandler is the unified shape every trigger action satisfies. Closures
// in actionHandlers adapt the underlying execute* methods (which take varying
// subsets of these args) to this signature, so adding a new action is one
// map entry plus its implementation, no switch arm to edit.
type actionHandler func(r *Runner, projectDB *sql.DB, event events.Event, config map[string]any) error

var actionHandlers = map[string]actionHandler{
	"send_email": func(r *Runner, pdb *sql.DB, e events.Event, c map[string]any) error {
		return r.executeSendEmail(pdb, e.ProjectID, c)
	},
	"http_request": func(r *Runner, _ *sql.DB, _ events.Event, c map[string]any) error {
		return r.executeHTTPRequest(c)
	},
	"insert_data": func(r *Runner, pdb *sql.DB, _ events.Event, c map[string]any) error {
		return r.executeInsertData(pdb, c)
	},
	"update_data": func(r *Runner, pdb *sql.DB, _ events.Event, c map[string]any) error {
		return r.executeUpdateData(pdb, c)
	},
	"trigger_webhook": func(r *Runner, pdb *sql.DB, e events.Event, c map[string]any) error {
		return r.executeTriggerWebhook(pdb, e, c)
	},
	"ws_broadcast": func(r *Runner, _ *sql.DB, e events.Event, c map[string]any) error {
		return r.executeWSBroadcast(e, c)
	},
	"run_sql": func(r *Runner, pdb *sql.DB, _ events.Event, c map[string]any) error {
		return r.executeRunSQL(pdb, c)
	},
	"enqueue_job": func(r *Runner, pdb *sql.DB, _ events.Event, c map[string]any) error {
		return r.executeEnqueueJob(pdb, c)
	},
	"run_function": func(r *Runner, pdb *sql.DB, e events.Event, c map[string]any) error {
		return r.executeRunFunction(pdb, e, c)
	},
	"send_notification": func(r *Runner, pdb *sql.DB, _ events.Event, c map[string]any) error {
		return r.executeSendNotification(pdb, c)
	},
	"send_push": func(r *Runner, pdb *sql.DB, e events.Event, c map[string]any) error {
		return r.executeSendPush(pdb, e.ProjectID, c)
	},
	"llm_call": func(r *Runner, pdb *sql.DB, e events.Event, c map[string]any) error {
		return r.executeLLMCall(pdb, e, c)
	},
	"ai_embed": func(r *Runner, pdb *sql.DB, e events.Event, c map[string]any) error {
		return r.executeAIEmbed(pdb, e, c)
	},
}

// executeSendEmail dispatches through the manage_email tool, which owns provider
// lookup, per-provider request shaping, template expansion, and auth injection.
// Config keys mirror the tool's send args; values are already resolved from the
// event payload by resolveConfigTemplates before this runs.
// Config: {"to":"{{email}}", "subject":"Welcome!", "template_name":"welcome", "template_vars":{"name":"{{username}}"}}
// Or:     {"to":"{{email}}", "subject":"...", "body_html":"<h1>Hello {{username}}</h1>"}
func (r *Runner) executeSendEmail(projectDB *sql.DB, projectID int, config map[string]any) error {
	// Send-arg shaping is shared with the job worker via internal/actionexec;
	// dispatch goes through runTool so the timeout and ToolContext stay uniform
	// across the messaging actions.
	return actionexec.SendEmail(config, func(toolName string, args map[string]any) error {
		return r.runTool(projectID, toolName, args)
	})
}

// executeHTTPRequest sends an HTTP request to an external URL via the executor
// shared with the job worker.
// Config: {"method":"POST", "url":"https://...", "headers":{"X-Key":"val"}, "body":{...}}
func (r *Runner) executeHTTPRequest(config map[string]any) error {
	ctx := r.lifecycle()
	opts := actionexec.HTTPOptions{
		DefaultMethod: http.MethodPost, // historical triggers default; jobs default to GET
	}
	if !r.allowPrivateHosts {
		opts.ValidateURL = netguard.ValidateExternalURL
	}
	return actionexec.HTTPRequest(ctx, r.client, config, opts)
}

// executeInsertData inserts a row into a table.
// Config: {"table":"audit_log", "data":{"user_id":"{{user_id}}", "action":"registered"}}
// The validated INSERT is built by internal/projectwrite (shared with the
// authoritative room actor's ctx.insert), so identifier validation and value
// binding live in exactly one place.
func (r *Runner) executeInsertData(projectDB *sql.DB, config map[string]any) error {
	table, _ := config["table"].(string)
	data, _ := config["data"].(map[string]any)
	if table == "" || len(data) == 0 {
		return fmt.Errorf("insert_data: 'table' and 'data' are required")
	}
	if _, err := projectwrite.Insert(projectDB, table, data); err != nil {
		return fmt.Errorf("insert_data: %w", err)
	}
	return nil
}

// executeUpdateData updates rows in a table matching a WHERE clause.
// Config: {"table":"users", "set":{"email_verified":true}, "where":{"id":"{{user_id}}"}}
// Increment support: {"set":{"likes":{"$increment":1}}} generates "likes = likes + 1"
// Decrement support: {"set":{"stock":{"$decrement":1}}} generates "stock = stock - 1"
// The validated UPDATE (including the $increment/$decrement operators) is built
// by internal/projectwrite, shared with the room actor's ctx.update.
func (r *Runner) executeUpdateData(projectDB *sql.DB, config map[string]any) error {
	table, _ := config["table"].(string)
	setData, _ := config["set"].(map[string]any)
	whereData, _ := config["where"].(map[string]any)
	if table == "" || len(setData) == 0 || len(whereData) == 0 {
		return fmt.Errorf("update_data: 'table', 'set', and 'where' are required (got table=%q, set keys=%d, where keys=%d)", table, len(setData), len(whereData))
	}
	if _, err := projectwrite.Update(projectDB, table, setData, whereData); err != nil {
		return fmt.Errorf("update_data: %w", err)
	}
	return nil
}

// executeTriggerWebhook sends a payload to an outgoing webhook by name.
// Config: {"webhook_name":"notify-slack", "payload":{"message":"Order placed"}}
func (r *Runner) executeTriggerWebhook(projectDB *sql.DB, event events.Event, config map[string]any) error {
	webhookName, _ := config["webhook_name"].(string)
	if webhookName == "" {
		return fmt.Errorf("trigger_webhook: 'webhook_name' is required")
	}

	var url, sealed string
	var isEnabled bool
	var webhookID int
	err := projectDB.QueryRow(
		"SELECT id, url, secret_encrypted, is_enabled FROM of_webhooks WHERE name = ? AND direction = 'outgoing'",
		webhookName,
	).Scan(&webhookID, &url, &sealed, &isEnabled)
	if err != nil {
		return fmt.Errorf("trigger_webhook: outgoing webhook '%s' not found", webhookName)
	}
	secret, err := r.encryptor.OpenOptional(sealed)
	if err != nil {
		return fmt.Errorf("trigger_webhook: the signing secret of '%s' cannot be decrypted: %w", webhookName, err)
	}
	if !isEnabled {
		return fmt.Errorf("trigger_webhook: webhook '%s' is disabled", webhookName)
	}
	if url == "" {
		return fmt.Errorf("trigger_webhook: webhook '%s' has no URL", webhookName)
	}

	// The body is the config's payload, else the raw event payload (no subscription
	// envelope): the trigger author chose what this receiver gets.
	payload := event.Payload
	if customPayload, ok := config["payload"].(map[string]any); ok {
		payload = customPayload
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("trigger_webhook: marshaling payload: %w", err)
	}
	if r.webhooks == nil {
		return fmt.Errorf("trigger_webhook: webhook sender not wired")
	}
	if err := r.webhooks.Send(webhooks.Target{ID: webhookID, Name: webhookName, URL: url, Secret: secret}, event.ProjectID, string(event.Type), bodyBytes); err != nil {
		return fmt.Errorf("trigger_webhook: %w", err)
	}
	return nil
}

// executeWSBroadcast sends a JSON message to a WebSocket room.
// Config: {"endpoint_path":"chat", "room":"general", "message":{"type":"new_order", ...}}
// endpoint_path is the BARE create-time path - no /api/ prefix and no /ws suffix,
// because that is the form the WS handler registers rooms under.
// The message is relayed to the browser byte-for-byte (no envelope, no _sender),
// and the ofnet helper dispatches on the frame's own `type`, so a message without
// one fires no handler anywhere. manage_triggers rejects that at create time; the
// no-message fallback below supplies one.
func (r *Runner) executeWSBroadcast(event events.Event, config map[string]any) error {
	if r.broadcaster == nil {
		return fmt.Errorf("ws_broadcast: WebSocket broadcaster not available")
	}

	endpointPath, _ := config["endpoint_path"].(string)
	room, _ := config["room"].(string)
	if endpointPath == "" {
		return fmt.Errorf("ws_broadcast: 'endpoint_path' is required")
	}
	// No room default: a client that connects without ?room= lands in the
	// empty-string room, and room keys compare exactly. Substituting "default"
	// here would broadcast into a room nobody occupies.

	// Build the message: use config["message"] if provided, otherwise the event
	// payload. The payload has no `type` of its own (it is the row's fields plus
	// table/endpoint/id), so stamp the event type onto a copy - otherwise the
	// fallback frame is undispatchable by construction and net.on() never fires.
	msg := config["message"]
	if msg == nil {
		wrapped := make(map[string]any, len(event.Payload)+1)
		for k, v := range event.Payload {
			wrapped[k] = v
		}
		wrapped["type"] = string(event.Type)
		msg = wrapped
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ws_broadcast: marshaling message: %w", err)
	}

	// Room key format: "projectID:tenantID:endpointPath:room". tenant_id is
	// optional; 0 lands in the non-tenanted bucket where anonymous clients
	// connect. Tenanted endpoints (those configured with tenant_column) must
	// pass tenant_id, typically templated as {{tenant_id}} from the row.
	tenantID := 0
	switch v := config["tenant_id"].(type) {
	case float64:
		tenantID = int(v)
	case int:
		tenantID = v
	case string:
		_, _ = fmt.Sscanf(v, "%d", &tenantID)
	}
	roomKey := events.RoomKey(event.ProjectID, tenantID, endpointPath, room)
	r.broadcaster.BroadcastToRoom(roomKey, msgBytes)

	return nil
}

// executeRunSQL runs a single INSERT/UPDATE/SELECT statement, optionally with
// bound parameters, and discards any result. Useful for computed aggregations
// triggered by events.
// Config: {"sql":"UPDATE stats SET order_count = (SELECT COUNT(*) FROM orders) WHERE id = ?", "params":[1]}
//
// SECURITY: the `sql` text is never templated (see execute) - event payload values
// must be passed through `params` and bound with ? placeholders, so they can never
// alter the statement. sqlguard.CheckAction refuses DDL, DELETE and stacked
// statements.
func (r *Runner) executeRunSQL(projectDB *sql.DB, config map[string]any) error {
	query, _ := config["sql"].(string)
	if _, err := sqlguard.CheckAction(query, "SELECT", "INSERT", "UPDATE"); err != nil {
		return fmt.Errorf("run_sql: %w", err)
	}
	if _, err := projectDB.Exec(query, runSQLParams(config["params"])...); err != nil {
		return fmt.Errorf("run_sql: %w", err)
	}
	return nil
}

// runSQLParams coerces a config["params"] value (a JSON array after template
// resolution) into a positional bind-arg slice. Anything that is not an array
// (absent, null, scalar) yields no params.
func runSQLParams(v any) []any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	return arr
}

// executeEnqueueJob creates a background job in the of_jobs queue.
// Config: {"type":"send_email", "payload":{"to":"user@example.com"}, "scheduled_at":"2024-01-01T00:00:00Z"}
func (r *Runner) executeEnqueueJob(projectDB *sql.DB, config map[string]any) error {
	jobType, _ := config["type"].(string)
	if jobType == "" {
		return fmt.Errorf("enqueue_job: 'type' is required")
	}

	payload := config["payload"]
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("enqueue_job: marshaling payload: %w", err)
	}

	scheduledAt := config["scheduled_at"]
	maxAttempts := 3
	if ma, ok := config["max_attempts"].(float64); ok && ma > 0 {
		maxAttempts = int(ma)
	}

	_, err = projectDB.Exec(
		`INSERT INTO of_jobs (type, payload, status, max_attempts, scheduled_at)
		 VALUES (?, ?, 'pending', ?, COALESCE(?, CURRENT_TIMESTAMP))`,
		jobType, string(payloadJSON), maxAttempts, scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("enqueue_job: %w", err)
	}
	return nil
}

// executeSendNotification creates in-app notifications.
// Config: {"user_id": "{{user_id}}", "title": "New order", "message": "...", "type": "info"}
func (r *Runner) executeSendNotification(projectDB *sql.DB, config map[string]any) error {
	title, _ := config["title"].(string)
	if title == "" {
		return fmt.Errorf("send_notification: title is required")
	}
	message, _ := config["message"].(string)
	notifType, _ := config["type"].(string)
	if notifType == "" {
		notifType = "info"
	}
	actionURL, _ := config["action_url"].(string)

	// Determine user_id (can be a number or string from template resolution).
	var userID int
	switch v := config["user_id"].(type) {
	case float64:
		userID = int(v)
	case string:
		_, _ = fmt.Sscanf(v, "%d", &userID)
	}

	_, err := projectDB.Exec(
		"INSERT INTO of_notifications (user_id, title, message, type, action_url) VALUES (?, ?, ?, ?, ?)",
		userID, title, message, notifType, actionURL,
	)
	return err
}
