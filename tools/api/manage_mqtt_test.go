package api

import (
	"testing"

	"openfabrica/tools/tooltest"
)

func TestManageMQTT_Lifecycle(t *testing.T) {
	h := tooltest.New(t)

	h.MustOK("manage_mqtt", map[string]any{
		"action":          "configure",
		"enabled":         true,
		"allow_anonymous": false,
		"max_clients":     50,
	})

	res := h.MustOK("manage_mqtt", map[string]any{
		"action":    "add_device",
		"name":      "esp_greenhouse",
		"username":  "esp1",
		"allow_pub": "sensors/esp1/#",
		"allow_sub": "actuators/esp1/#",
	})
	data := res.Data.(map[string]any)
	if tok, _ := data["token"].(string); tok == "" {
		t.Fatal("expected generated device token")
	}

	h.MustOK("manage_mqtt", map[string]any{
		"action":       "add_rule",
		"name":         "ingest_temp",
		"topic_filter": "sensors/+/temp",
		"target_table": "readings",
	})

	h.MustOK("manage_mqtt", map[string]any{
		"action":  "publish",
		"topic":   "sensors/esp1/temp",
		"payload": `{"c":23.4}`,
		"retain":  true,
	})

	res = h.MustOK("manage_mqtt", map[string]any{"action": "status"})
	st := res.Data.(map[string]any)
	if enabled, _ := st["enabled"].(bool); !enabled {
		t.Fatal("expected enabled = true")
	}
}
