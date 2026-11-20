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
		"user_allow_pub":  "commands/user/{user_id}/#",
		"user_allow_sub":  "sensors/#",
	})

	res := h.MustOK("manage_mqtt", map[string]any{
		"action":        "add_device",
		"name":          "esp_greenhouse",
		"username":      "esp1",
		"allow_pub":     "sensors/esp1/#",
		"allow_sub":     "actuators/esp1/#",
		"heartbeat_sec": 300,
	})
	data := res.Data.(map[string]any)
	if tok, _ := data["token"].(string); tok == "" {
		t.Fatal("expected generated device token")
	}

	h.MustOK("manage_mqtt", map[string]any{
		"action":           "add_rule",
		"name":             "ingest_temp",
		"topic_filter":     "sensors/{zone}/temp",
		"target_table":     "readings",
		"min_interval_sec": 10,
		"retention_days":   7,
		"rollup_interval":  "1h",
	})

	h.MustFail("manage_mqtt", map[string]any{
		"action":          "add_rule",
		"name":            "bad_rollup",
		"topic_filter":    "sensors/+/temp",
		"rollup_interval": "7m",
	}, "")

	h.MustOK("manage_mqtt", map[string]any{
		"action":   "rollups",
		"rule":     "ingest_temp",
		"interval": "1h",
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
	if got, _ := st["user_allow_pub"].(string); got != "commands/user/{user_id}/#" {
		t.Fatalf("expected user_allow_pub persisted, got %q", got)
	}
	if got, _ := st["user_allow_sub"].(string); got != "sensors/#" {
		t.Fatalf("expected user_allow_sub persisted, got %q", got)
	}

	h.MustFail("manage_mqtt", map[string]any{
		"action":         "configure",
		"enabled":        true,
		"user_allow_pub": "sensors/#/invalid",
	}, "")

	// Outbound bridge lifecycle & validation.
	h.MustOK("manage_mqtt", map[string]any{
		"action":       "add_bridge",
		"name":         "home_mosquitto",
		"broker_url":   "tcp://192.168.1.50:1883",
		"remote_sub":   "zigbee2mqtt/#,frigate/events",
		"local_prefix": "lan/",
		"local_pub":    "lan/zigbee2mqtt/+/set",
		"username":     "ha",
		"password":     "super-secret-lan-pw",
		"qos":          1,
	})

	h.MustFail("manage_mqtt", map[string]any{
		"action":     "add_bridge",
		"name":       "bad_meta",
		"broker_url": "tcp://169.254.169.254:1883",
		"remote_sub": "#",
	}, "")

	res = h.MustOK("manage_mqtt", map[string]any{"action": "status"})
	st = res.Data.(map[string]any)
	bridges, _ := st["bridges"].([]map[string]any)
	if len(bridges) != 1 || bridges[0]["name"] != "home_mosquitto" {
		t.Fatalf("expected 1 bridge in status, got %#v", st["bridges"])
	}
	if _, leaked := bridges[0]["password"]; leaked {
		t.Fatal("bridge password must not be returned in status")
	}

	h.MustOK("manage_mqtt", map[string]any{
		"action": "delete_bridge",
		"name":   "home_mosquitto",
	})
}
