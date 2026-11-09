package automation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/tools"
)

// manage_triggers registers server-side event hooks fired off the event bus.
// Every trigger runs asynchronously AFTER the write that raised the event has
// committed (triggers/runner.go); there is no synchronous pre-commit hook, so a
// trigger cannot validate, rewrite, or reject a row before it persists.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_triggers",
		Category: "automation",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Define server-side event hooks (auth, data, payment, webhook) with filters and {{template}} vars.",
		WhenToUse: `Use whenever the project says "when X happens, do Y" - signup welcome email,
order confirmation, low-stock alert. Triggers run server-side from the event
bus; they always beat client-side glue. Every trigger fires asynchronously
AFTER the write commits - there is no pre-commit hook, so to set a column before
insert do it in the client or the manage_function endpoint that writes the row.`,
		Actions: []tools.Action{
			&TriggerCreate{},
			&TriggerList{},
			&TriggerUpdate{},
			&TriggerDelete{},
			&TriggerTest{},
		},
		Summarize: triggerSummarize,
	})
}

// validTriggerTypes is the closed set of side-effect kinds a trigger may
// declare. All of them run as "after" actions off the event bus. There are no
// validate/transform/reject_if "before" types: the runner has no handler for
// them and never reads the timing column, so they would be silent no-ops.
var validTriggerTypes = map[string]bool{
	"send_email": true, "send_push": true, "send_notification": true,
	"http_request": true, "insert_data": true, "update_data": true,
	"trigger_webhook": true, "ws_broadcast": true, "run_sql": true, "enqueue_job": true,
	"llm_call": true, "ai_embed": true, "run_function": true,
}

// beforeHookReject explains why a pre-commit / before-hook request cannot be
// honored and points at the supported alternatives. Kept as one string so the
// create and update paths reject identically.
const beforeHookReject = "before-hook triggers are not supported: the runner fires every trigger asynchronously AFTER the write commits, so a trigger cannot validate, transform, or reject a row before it persists. To set a column before insert, do it in the client or in the manage_function endpoint that creates the row; to react after a write, use an 'after' trigger (run_function, run_sql, insert_data, ...)."

// wsBroadcastConfigError validates a ws_broadcast config before it is stored.
// A ws_broadcast frame reaches the browser EXACTLY as written - the hub relays
// the bytes with no envelope - and the ofnet helper dispatches on the frame's
// own `type` field. So a message without `type` fires no net.on() handler
// anywhere while the trigger still reports success: the silent failure this
// check exists to convert into a build error. Returns "" when the config is
// fine.
func wsBroadcastConfigError(cfg map[string]any) string {
	if cfg == nil {
		return ""
	}
	msg, present := cfg["message"]
	if !present || msg == nil {
		// The no-message fallback broadcasts the event payload, which the runner
		// wraps so it carries a `type`. Nothing to check here.
		return ""
	}
	obj, ok := msg.(map[string]any)
	if !ok {
		return "ws_broadcast: 'message' must be a JSON object carrying a 'type' field - the frame is relayed to the browser verbatim and clients dispatch on it via net.on('<type>', ...)."
	}
	t, _ := obj["type"].(string)
	if strings.TrimSpace(t) == "" {
		return "ws_broadcast: 'message' must carry a non-empty string 'type' (e.g. {\"type\":\"new_order\", ...}). The frame is relayed to the browser verbatim and the client receives it with net.on('new_order', fn); without 'type' no handler ever fires and the trigger still reports success."
	}
	return ""
}

// TriggerCreate registers a new trigger. trigger_config is a free-form JSON
// object whose shape depends on trigger_type; {{field}} placeholders inside
// it are expanded against the event payload at fire time.
type TriggerCreate struct {
	Name          string         `arg:"name,required"           desc:"Trigger name (unique)."`
	EventType     string         `arg:"event_type,required"     desc:"Event to fire on. Platform-raised events: data.insert, data.update, data.delete, auth.register, auth.login, payment.completed, payment.failed, webhook.received, notification.created, tenant.created, tenant.member_invited, tenant.member_joined, tenant.switched; a custom name a manage_schedules task publishes also fires. A misspelled name matches nothing and the trigger silently never runs. The event_actions guide documents each event's payload fields."`
	TriggerType   string         `arg:"trigger_type,required"   desc:"Side effect to run after the write commits. run_function invokes a manage_function handler with the event payload as req.body - reach for it when the reaction needs computed logic (an external API call with response shaping, multi-step validate-then-write) the other action types can't express." enum:"send_email,send_push,send_notification,http_request,insert_data,update_data,trigger_webhook,ws_broadcast,run_sql,enqueue_job,llm_call,ai_embed,run_function"`
	TriggerConfig map[string]any `arg:"trigger_config,required" desc:"Side-effect config; supports {{template}} variables from event payload. EXCEPTION: for trigger_type=run_sql, put payload values in a params array and use ? placeholders in sql (e.g. {\"sql\":\"UPDATE t SET n=n+1 WHERE id=?\",\"params\":[\"{{id}}\"]}); never put {{...}} inside the sql string. run_sql allows only single SELECT/INSERT/UPDATE statements. For trigger_type=run_function, config is {\"endpoint_path\":\"<function path>\"} and the whole event payload is passed to the handler as req.body."`
	EventFilter   map[string]any `arg:"event_filter"            desc:"All keys must match the event payload (e.g. {\"table\":\"users\"}). On data.* events \"table\" is the PHYSICAL TABLE the row lives in (the table_name you created), not the endpoint's route; filter on \"endpoint\" to single out one endpoint when several serve the same table."`
	Timing        string         `arg:"timing"                  desc:"Always 'after' (async; fires after the write commits). Synchronous before-hooks / pre-commit interception are not supported." default:"after"`
}

func (a *TriggerCreate) Doc() string { return "Register a new trigger." }
func (a *TriggerCreate) Example() any {
	return &TriggerCreate{
		Name: "welcome_email", EventType: "auth.register", TriggerType: "send_email",
		TriggerConfig: map[string]any{"to": "{{email}}", "subject": "Welcome", "body_html": "<p>Thanks for joining.</p>"},
	}
}
func (a *TriggerCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if t := strings.TrimSpace(a.Timing); t != "" && !strings.EqualFold(t, "after") {
		return tools.Fail(beforeHookReject), nil
	}
	if !validTriggerTypes[a.TriggerType] {
		return tools.Fail("invalid trigger_type '%s'", a.TriggerType), nil
	}
	if a.TriggerType == "ws_broadcast" {
		if msg := wsBroadcastConfigError(a.TriggerConfig); msg != "" {
			return tools.Fail("%s", msg), nil
		}
	}
	if msg := triggerConfigError(a.TriggerType, a.TriggerConfig); msg != "" {
		return tools.Fail("%s", msg), nil
	}
	// Names key update and delete, but the table does not enforce them unique, so a
	// duplicate would leave every later lookup ambiguous.
	var taken int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_triggers WHERE name = ?", a.Name).Scan(&taken)
	if taken > 0 {
		return tools.FailCode("conflict", "a trigger named %q already exists: use manage_triggers update to change it, or pick another name", a.Name), nil
	}
	timing := "after"
	configJSON, err := json.Marshal(a.TriggerConfig)
	if err != nil {
		return tools.Fail("invalid trigger_config JSON"), nil
	}
	var filterJSON *string
	if a.EventFilter != nil {
		data, err := json.Marshal(a.EventFilter)
		if err != nil {
			return tools.Fail("invalid event_filter JSON: %s", err.Error()), nil
		}
		s := string(data)
		filterJSON = &s
	}
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_triggers (name, event_type, event_filter, trigger_type, trigger_config, timing)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		a.Name, a.EventType, filterJSON, a.TriggerType, string(configJSON), timing,
	); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return tools.FailCode("conflict", "a trigger named %q already exists: use manage_triggers update to change it, or pick another name", a.Name), nil
		}
		return nil, fmt.Errorf("creating trigger: %w", err)
	}
	data := map[string]any{
		"name": a.Name, "event_type": a.EventType, "trigger_type": a.TriggerType,
	}
	if w := triggerWarnings(ctx, a.EventType, a.TriggerType, a.TriggerConfig, a.EventFilter); len(w) > 0 {
		data["warnings"] = w
	}
	return tools.Ok(data), nil
}

// TriggerList returns every registered trigger ordered by name.
type TriggerList struct{}

func (a *TriggerList) Doc() string    { return "List every registered trigger." }
func (a *TriggerList) ReadOnly() bool { return true }
func (a *TriggerList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT id, name, event_type, event_filter, trigger_type, trigger_config, is_enabled, created_at FROM of_triggers ORDER BY name",
	)
	if err != nil {
		return nil, fmt.Errorf("listing triggers: %w", err)
	}
	defer rows.Close()
	var triggers []map[string]any
	for rows.Next() {
		var id int
		var name, eventType, triggerType, triggerConfig string
		var eventFilter sql.NullString
		var isEnabled bool
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &eventType, &eventFilter, &triggerType, &triggerConfig, &isEnabled, &createdAt); err != nil {
			continue
		}
		entry := map[string]any{
			"id": id, "name": name, "event_type": eventType,
			"trigger_type": triggerType, "trigger_config": triggerConfig,
			"is_enabled": isEnabled, "created_at": createdAt,
		}
		if eventFilter.Valid {
			entry["event_filter"] = eventFilter.String
		}
		triggers = append(triggers, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing triggers: %w", err)
	}
	return tools.Ok(triggers), nil
}

// TriggerUpdate patches one or more fields of an existing trigger.
type TriggerUpdate struct {
	Name          string         `arg:"name,required"   desc:"Trigger name (key for lookup)."`
	EventType     string         `arg:"event_type"      desc:"New event type."`
	TriggerType   string         `arg:"trigger_type"    desc:"New side-effect kind."`
	TriggerConfig map[string]any `arg:"trigger_config"  desc:"New side-effect config."`
	EventFilter   map[string]any `arg:"event_filter"    desc:"New filter."`
	IsEnabled     *bool          `arg:"is_enabled"      desc:"Enable or disable."`
}

func (a *TriggerUpdate) Doc() string { return "Update a trigger's config or enable flag." }
func (a *TriggerUpdate) Example() any {
	enabled := false
	return &TriggerUpdate{Name: "welcome_email", IsEnabled: &enabled}
}
func (a *TriggerUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var id int
	if err := ctx.DB.QueryRow("SELECT id FROM of_triggers WHERE name = ?", a.Name).Scan(&id); err != nil {
		return tools.Fail("trigger '%s' not found", a.Name), nil
	}

	sets := []string{}
	vals := []any{}
	if a.EventType != "" {
		sets = append(sets, "event_type = ?")
		vals = append(vals, a.EventType)
	}
	if a.TriggerType != "" {
		if !validTriggerTypes[a.TriggerType] {
			return tools.Fail("invalid trigger_type '%s'", a.TriggerType), nil
		}
		sets = append(sets, "trigger_type = ?")
		vals = append(vals, a.TriggerType)
	}
	// A type change re-validates the stored config, and a config change validates the
	// new one against the type it will run under, so neither slips past the create check.
	if a.TriggerType != "" || a.TriggerConfig != nil {
		effType, effConfig := a.TriggerType, a.TriggerConfig
		if effType == "" {
			_ = ctx.DB.QueryRow("SELECT trigger_type FROM of_triggers WHERE name = ?", a.Name).Scan(&effType)
		}
		if effConfig == nil {
			var stored string
			_ = ctx.DB.QueryRow("SELECT trigger_config FROM of_triggers WHERE name = ?", a.Name).Scan(&stored)
			_ = json.Unmarshal([]byte(stored), &effConfig)
		}
		if msg := triggerConfigError(effType, effConfig); msg != "" {
			return tools.Fail("%s", msg), nil
		}
	}
	if a.TriggerConfig != nil {
		data, err := json.Marshal(a.TriggerConfig)
		if err != nil {
			return tools.Fail("invalid trigger_config JSON: %s", err.Error()), nil
		}
		// The type this config will run under is the one being set, or the stored
		// one when the update only replaces the config. Without that lookup a
		// ws_broadcast trigger could be re-pointed at a type-less message here and
		// skip the create-time check.
		effectiveType := a.TriggerType
		if effectiveType == "" {
			_ = ctx.DB.QueryRow("SELECT trigger_type FROM of_triggers WHERE name = ?", a.Name).Scan(&effectiveType)
		}
		if effectiveType == "ws_broadcast" {
			if msg := wsBroadcastConfigError(a.TriggerConfig); msg != "" {
				return tools.Fail("%s", msg), nil
			}
		}
		sets = append(sets, "trigger_config = ?")
		vals = append(vals, string(data))
	}
	if a.EventFilter != nil {
		data, err := json.Marshal(a.EventFilter)
		if err != nil {
			return tools.Fail("invalid event_filter JSON: %s", err.Error()), nil
		}
		sets = append(sets, "event_filter = ?")
		vals = append(vals, string(data))
	}
	if a.IsEnabled != nil {
		sets = append(sets, "is_enabled = ?")
		vals = append(vals, *a.IsEnabled)
	}
	if len(sets) == 0 {
		return tools.Fail("nothing to update"), nil
	}
	vals = append(vals, id)
	query := fmt.Sprintf("UPDATE of_triggers SET %s WHERE id = ?", strings.Join(sets, ", "))
	if _, err := ctx.DB.Exec(query, vals...); err != nil {
		return nil, fmt.Errorf("updating trigger: %w", err)
	}
	return tools.Ok(map[string]any{"name": a.Name, "updated": true}), nil
}

// TriggerDelete removes a trigger by name.
type TriggerDelete struct {
	Name string `arg:"name,required" desc:"Trigger name to delete."`
}

func (a *TriggerDelete) Doc() string  { return "Delete a trigger by name." }
func (a *TriggerDelete) Example() any { return &TriggerDelete{Name: "welcome_email"} }
func (a *TriggerDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	res, err := ctx.DB.Exec("DELETE FROM of_triggers WHERE name = ?", a.Name)
	if err != nil {
		return nil, fmt.Errorf("deleting trigger: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return tools.Fail("trigger '%s' not found", a.Name), nil
	}
	return tools.Ok(map[string]any{"name": a.Name, "deleted": true}), nil
}

// TriggerTest dry-runs a trigger against a simulated event payload: reports
// whether the filter matches and what the resolved config would be after
// {{template}} expansion. No side effects.
type TriggerTest struct {
	Name        string         `arg:"name,required"         desc:"Trigger name."`
	TestPayload map[string]any `arg:"test_payload,required" desc:"Simulated event payload for the dry-run."`
}

func (a *TriggerTest) Doc() string    { return "Dry-run a trigger against a simulated event payload." }
func (a *TriggerTest) ReadOnly() bool { return true }
func (a *TriggerTest) Example() any {
	return &TriggerTest{Name: "welcome_email", TestPayload: map[string]any{"email": "test@example.com"}}
}
func (a *TriggerTest) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var triggerConfig, eventFilter sql.NullString
	var triggerType string
	err := ctx.DB.QueryRow(
		"SELECT trigger_type, trigger_config, event_filter FROM of_triggers WHERE name = ?", a.Name,
	).Scan(&triggerType, &triggerConfig, &eventFilter)
	if err != nil {
		return tools.Fail("trigger '%s' not found", a.Name), nil
	}
	filterMatch := true
	if eventFilter.Valid && eventFilter.String != "" {
		var filter map[string]any
		if err := json.Unmarshal([]byte(eventFilter.String), &filter); err == nil {
			for key, expected := range filter {
				actual, ok := a.TestPayload[key]
				if !ok || fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", expected) {
					filterMatch = false
					break
				}
			}
		}
	}
	resolved := triggerConfig.String
	if triggerConfig.Valid {
		resolved = tools.ResolveTemplateVars(triggerConfig.String, a.TestPayload)
	}
	return tools.Ok(map[string]any{
		"trigger_type":    triggerType,
		"filter_match":    filterMatch,
		"resolved_config": resolved,
		"note":            "Dry run, no trigger was executed",
	}), nil
}
