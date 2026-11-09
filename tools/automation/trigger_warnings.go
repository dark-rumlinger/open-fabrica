package automation

import (
	"fmt"
	"strings"

	"openfabrica/events"
	"openfabrica/tools"
)

// platformTriggerEvents are the events the platform itself raises for a project's
// triggers (the trigger-events contract's catalogue). Matching is exact string equality,
// so a name outside this set fires only if a scheduled trigger_event task publishes it.
var platformTriggerEvents = []string{
	string(events.EventDataInsert), string(events.EventDataUpdate), string(events.EventDataDelete),
	string(events.EventAuthRegister), string(events.EventAuthLogin),
	string(events.EventPaymentCompleted), string(events.EventPaymentFailed),
	string(events.EventWebhookReceived), string(events.EventNotificationCreated),
	string(events.EventTenantCreated), string(events.EventTenantMemberInvited),
	string(events.EventTenantMemberJoined), string(events.EventTenantSwitched),
}

// triggerWarnings reports what a saved trigger will do that its author probably did not
// intend: an event nothing raises, a data.* trigger with no filter, or a target that does
// not exist yet. The trigger is still saved (the target may be created later in the same
// build), so these are advice, not refusals.
func triggerWarnings(ctx *tools.ToolContext, eventType, triggerType string, config, filter map[string]any) []string {
	var out []string
	known := false
	for _, e := range platformTriggerEvents {
		if e == eventType {
			known = true
			break
		}
	}
	if !known {
		var n int
		_ = ctx.DB.QueryRow(
			`SELECT COUNT(*) FROM of_scheduled_tasks WHERE native_action LIKE ?`,
			`%"event_type"%`+eventType+`%`).Scan(&n)
		if n == 0 {
			out = append(out, fmt.Sprintf("event_type %q is not an event the platform raises (%s) and no scheduled trigger_event task publishes it, so this trigger will never fire unless you add one. Check the spelling.",
				eventType, strings.Join(platformTriggerEvents, ", ")))
		}
	}
	if strings.HasPrefix(eventType, "data.") && len(filter) == 0 {
		out = append(out, "no event_filter on a data.* trigger: it fires for a row change in EVERY table. Add event_filter {\"table\":\"<table>\"} (or \"endpoint\") to narrow it.")
	}
	switch triggerType {
	case "run_function":
		if p, _ := config["endpoint_path"].(string); p != "" && !strings.Contains(p, "{{") {
			var n int
			_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM of_function_endpoints WHERE path = ?`, tools.NormalizeEndpointPath(p)).Scan(&n)
			if n == 0 {
				out = append(out, fmt.Sprintf("no function endpoint exists at %q yet (manage_function create); the trigger fails every time until it does.", p))
			}
		}
	case "trigger_webhook":
		if name, _ := config["webhook_name"].(string); name != "" && !strings.Contains(name, "{{") {
			var n int
			_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM of_webhooks WHERE name = ?`, name).Scan(&n)
			if n == 0 {
				out = append(out, fmt.Sprintf("no webhook named %q exists yet (manage_webhooks create).", name))
			}
		}
	case "insert_data", "update_data":
		if t, _ := config["table"].(string); t != "" && !strings.Contains(t, "{{") {
			var n int
			_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?`, t).Scan(&n)
			if n == 0 {
				out = append(out, fmt.Sprintf("table %q does not exist yet (manage_schema create_table).", t))
			}
		}
	}
	return out
}
