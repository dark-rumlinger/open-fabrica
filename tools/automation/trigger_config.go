package automation

import (
	"fmt"
	"strings"
)

// triggerRequiredKeys are the config keys each trigger type cannot run without.
// Checked when a trigger is saved, so a wrong shape fails with the tool's message
// instead of only in the server log when the event first fires.
var triggerRequiredKeys = map[string][]string{
	"send_email":        {"to"},
	"send_push":         {"endpoint_path", "title"},
	"send_notification": {"title"},
	"http_request":      {"url"},
	"insert_data":       {"table", "data"},
	"update_data":       {"table", "set", "where"},
	"trigger_webhook":   {"webhook_name"},
	"run_sql":           {"sql"},
	"enqueue_job":       {"type"},
	"llm_call":          {"prompt"},
	"run_function":      {"endpoint_path"},
	"ws_broadcast":      {"endpoint_path"},
}

// enqueueJobTypes are the job types the queue worker executes; any other type fails
// with "unknown job type" after its retries, so it is refused on save instead.
var enqueueJobTypes = []string{"send_email", "http_request", "run_sql", "llm_prompt"}

// triggerConfigError reports a config that cannot work for its trigger type, or "".
func triggerConfigError(triggerType string, config map[string]any) string {
	missing := []string{}
	for _, key := range triggerRequiredKeys[triggerType] {
		if v, ok := config[key]; !ok || v == nil || v == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("trigger_type %s needs %s in trigger_config", triggerType, strings.Join(missing, ", "))
	}
	switch triggerType {
	case "send_email":
		// The fire-time send goes through manage_email, which needs a subject and a
		// body unless a saved template supplies them; checked here so the trigger
		// fails on save, not silently in the server log on every event.
		if tmpl, _ := config["template_name"].(string); tmpl == "" {
			subject, _ := config["subject"].(string)
			html, _ := config["body_html"].(string)
			text, _ := config["body_text"].(string)
			if subject == "" || (html == "" && text == "") {
				return "trigger_type send_email needs subject plus body_html or body_text in trigger_config (or a saved template_name) - manage_email rejects a send without them"
			}
		}
	case "send_push":
		// The tool's send arg is message_body; the trigger config key is body.
		if _, ok := config["message_body"]; ok {
			return fmt.Sprintf("trigger_type %s takes the text as `body` (message_body is the manage_%s tool argument, not a trigger_config key)", triggerType, strings.TrimPrefix(triggerType, "send_"))
		}
	case "run_sql":
		// Templating is refused at fire time; checked here so the trigger fails on
		// save instead of silently never working.
		if query, _ := config["sql"].(string); strings.Contains(query, "{{") {
			return "run_sql does not allow {{...}} templating in sql; pass payload values in params and use ? placeholders, e.g. {\"sql\":\"UPDATE stats SET n = n + 1 WHERE id = ?\",\"params\":[\"{{id}}\"]}"
		}
	case "enqueue_job":
		if jobType, _ := config["type"].(string); jobType != "" && !strings.Contains(jobType, "{{") {
			known := false
			for _, t := range enqueueJobTypes {
				if t == jobType {
					known = true
					break
				}
			}
			if !known {
				return fmt.Sprintf("enqueue_job type %q is not a job type the queue runs; use one of %s", jobType, strings.Join(enqueueJobTypes, ", "))
			}
		}
	case "llm_call":
		if table, _ := config["save_to_table"].(string); table != "" {
			if col, _ := config["save_to_column"].(string); col == "" {
				return "llm_call with save_to_table also needs save_to_column"
			}
			if where, _ := config["save_where"].(map[string]any); len(where) == 0 {
				return "llm_call with save_to_table also needs save_where, e.g. {\"id\":\"{{id}}\"}, to pick the row that receives the reply"
			}
		}
	}
	return ""
}
