package automation

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupTriggerDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupTriggerDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

func createTrigger(ctx *tools.ToolContext, name, eventType, triggerType, timing string) (*tools.Result, error) {
	a := &TriggerCreate{
		Name:          name,
		EventType:     eventType,
		TriggerType:   triggerType,
		TriggerConfig: sampleTriggerConfig(triggerType),
		Timing:        timing,
	}
	return a.Run(ctx)
}

// sampleTriggerConfig is a minimal valid config for each trigger type.
func sampleTriggerConfig(triggerType string) map[string]any {
	switch triggerType {
	case "run_function":
		return map[string]any{"endpoint_path": "fn"}
	case "run_sql":
		return map[string]any{"sql": "UPDATE stats SET n = n + 1"}
	case "insert_data":
		return map[string]any{"table": "t", "data": map[string]any{"a": "{{id}}"}}
	}
	return map[string]any{"to": "{{email}}", "subject": "Hi", "body_text": "Welcome"}
}

// TestTriggerCreate_RejectsBeforeTiming: the runner fires every trigger async
// after commit, so timing="before" (a synchronous pre-commit hook) is not
// supported and must be rejected with guidance rather than silently stored.
func TestTriggerCreate_RejectsBeforeTiming(t *testing.T) {
	db := setupTriggerDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := createTrigger(ctx, "gate", "data.insert", "send_email", "before")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection of timing=before")
	}
	if !strings.Contains(res.Error, "before-hook") {
		t.Errorf("expected before-hook guidance, got %q", res.Error)
	}
}

// TestTriggerCreate_BeforeTimingReachesHandlerGuidance goes through the
// registered Execute path (alias/validation included): timing must not carry an
// enum tag, or framework enum validation would reject "before" with a generic
// message before the handler's teaching rejection can fire.
func TestTriggerCreate_BeforeTimingReachesHandlerGuidance(t *testing.T) {
	db := setupTriggerDB(t)
	ctx := &tools.ToolContext{DB: db}
	def, err := tools.DefaultRegistry().Get("manage_triggers")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	res, err := def.Execute(ctx, map[string]any{
		"action":         "create",
		"name":           "gate",
		"event_type":     "data.insert",
		"trigger_type":   "send_email",
		"trigger_config": map[string]any{"to": "{{email}}"},
		"timing":         "before",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection of timing=before")
	}
	if !strings.Contains(res.Error, "before-hook") {
		t.Errorf("expected the before-hook guidance to reach the caller, got %q", res.Error)
	}
}

// TestTriggerCreate_RejectsUnimplementedTypes: validate/transform/reject_if had
// no runner handler (silent no-ops) and are no longer valid trigger types.
func TestTriggerCreate_RejectsUnimplementedTypes(t *testing.T) {
	db := setupTriggerDB(t)
	ctx := &tools.ToolContext{DB: db}
	for _, tt := range []string{"transform", "validate", "reject_if"} {
		res, err := createTrigger(ctx, "t_"+tt, "data.insert", tt, "after")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt, err)
		}
		if res.Success {
			t.Errorf("%s: expected rejection of removed trigger_type", tt)
		}
	}
}

// TestTriggerCreate_AcceptsAfterActions: the supported after-actions still work
// and are stored with timing="after".
func TestTriggerCreate_AcceptsAfterActions(t *testing.T) {
	db := setupTriggerDB(t)
	ctx := &tools.ToolContext{DB: db}
	for _, tt := range []string{"send_email", "run_function", "run_sql", "insert_data"} {
		res, err := createTrigger(ctx, "ok_"+tt, "data.insert", tt, "")
		if err != nil || res == nil || !res.Success {
			t.Fatalf("%s: expected success, got err=%v res=%+v", tt, err, res)
		}
	}
	var timing string
	if err := db.QueryRow("SELECT timing FROM of_triggers WHERE name = ?", "ok_send_email").Scan(&timing); err != nil {
		t.Fatalf("load: %v", err)
	}
	if timing != "after" {
		t.Errorf("expected timing=after, got %q", timing)
	}
}

// TestTriggerUpdate_RejectsUnimplementedType: the update path also validates the
// trigger_type so a trigger can't be flipped to a removed before-hook type.
func TestTriggerUpdate_RejectsUnimplementedType(t *testing.T) {
	db := setupTriggerDB(t)
	ctx := &tools.ToolContext{DB: db}
	if res, err := createTrigger(ctx, "welcome", "auth.register", "send_email", ""); err != nil || !res.Success {
		t.Fatalf("seed create: err=%v res=%+v", err, res)
	}
	upd := &TriggerUpdate{Name: "welcome", TriggerType: "transform"}
	res, err := upd.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection updating to a removed trigger_type")
	}
}

func TestTriggerConfigError(t *testing.T) {
	cases := []struct {
		name   string
		typ    string
		config map[string]any
		want   string // substring of the message; "" means valid
	}{
		{"email ok", "send_email", map[string]any{"to": "{{email}}", "subject": "Hi", "body_html": "<p>x</p>"}, ""},
		{"email template ok", "send_email", map[string]any{"to": "{{email}}", "template_name": "welcome"}, ""},
		{"email missing body", "send_email", map[string]any{"to": "{{email}}", "subject": "Hi"}, "needs subject plus body_html"},
		{"ws_broadcast missing path", "ws_broadcast", map[string]any{"message": map[string]any{"type": "x"}}, "needs endpoint_path"},
		{"email missing to", "send_email", map[string]any{"subject": "Hi"}, "needs to"},
		{"push uses the tool arg name", "send_push", map[string]any{"endpoint_path": "news", "title": "T", "message_body": "x"}, "takes the text as `body`"},
		{"push missing title", "send_push", map[string]any{"endpoint_path": "news", "body": "x"}, "needs title"},
		{"update_data missing where", "update_data", map[string]any{"table": "t", "set": map[string]any{"a": 1}}, "needs where"},
		{"llm_call prompt only", "llm_call", map[string]any{"prompt": "summarise {{body}}"}, ""},
		{"llm_call save without column", "llm_call", map[string]any{"prompt": "p", "save_to_table": "posts"}, "save_to_column"},
		{"llm_call save without where", "llm_call", map[string]any{"prompt": "p", "save_to_table": "posts", "save_to_column": "summary"}, "save_where"},
		{"llm_call full", "llm_call", map[string]any{"prompt": "p", "save_to_table": "posts", "save_to_column": "summary", "save_where": map[string]any{"id": "{{id}}"}}, ""},
		{"ai_embed needs nothing", "ai_embed", map[string]any{}, ""},
		{"run_sql with params ok", "run_sql", map[string]any{"sql": "UPDATE s SET n = n + 1 WHERE id = ?", "params": []any{"{{id}}"}}, ""},
		{"run_sql templated sql", "run_sql", map[string]any{"sql": "UPDATE s SET n = n + 1 WHERE id = {{id}}"}, "does not allow {{...}}"},
		{"enqueue_job known type", "enqueue_job", map[string]any{"type": "send_email", "payload": map[string]any{}}, ""},
		{"enqueue_job unknown type", "enqueue_job", map[string]any{"type": "send_sms"}, "not a job type the queue runs"},
	}
	for _, c := range cases {
		got := triggerConfigError(c.typ, c.config)
		if c.want == "" && got != "" {
			t.Errorf("%s: unexpected error %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: error %q does not contain %q", c.name, got, c.want)
		}
	}
}

// Changing only the type must re-check the stored config against the new type.
func TestTriggerUpdate_TypeChangeRevalidatesStoredConfig(t *testing.T) {
	db := setupTriggerDB(t)
	ctx := &tools.ToolContext{DB: db}
	if res, err := createTrigger(ctx, "t1", "data.insert", "send_email", ""); err != nil || !res.Success {
		t.Fatalf("create: %v %+v", err, res)
	}
	res, err := (&TriggerUpdate{Name: "t1", TriggerType: "http_request"}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success {
		t.Fatal("re-typing a trigger to http_request kept a config with no url")
	}
}

// A trigger on an event nothing raises never fires; saving one now says so, and a
// data.* trigger with no filter says it fires for every table. A duplicate name is a
// readable conflict, not a raw SQL error.
func TestTriggerCreate_WarnsAndReportsConflict(t *testing.T) {
	h := tooltest.New(t)
	args := map[string]any{
		"action": "create", "name": "typo", "event_type": "data.insrt", "trigger_type": "send_notification",
		"trigger_config": map[string]any{"title": "hi"},
	}
	res := h.MustOK("manage_triggers", args)
	data, _ := res.Data.(map[string]any)
	if w, _ := data["warnings"].([]string); len(w) == 0 || !strings.Contains(w[0], "data.insrt") {
		t.Fatalf("expected an unknown-event warning, got %v", data)
	}

	broad := h.MustOK("manage_triggers", map[string]any{
		"action": "create", "name": "all_rows", "event_type": "data.insert", "trigger_type": "send_notification",
		"trigger_config": map[string]any{"title": "hi"},
	})
	if w, _ := broad.Data.(map[string]any)["warnings"].([]string); len(w) != 1 || !strings.Contains(w[0], "EVERY table") {
		t.Fatalf("expected the unfiltered data.* warning, got %v", broad.Data)
	}

	dup := h.MustFail("manage_triggers", args, "conflict")
	if !strings.Contains(dup.Error, "already exists") {
		t.Fatalf("duplicate error = %q", dup.Error)
	}
}
