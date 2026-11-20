package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdminProjectMQTT_Lifecycle(t *testing.T) {
	d := newBotTestDeps(t)
	if _, err := d.ProjectDBManager.Open(5); err != nil {
		t.Fatal(err)
	}
	h := &ProjectMQTTHandler{deps: d}
	proj := map[string]string{"projectID": "5"}

	// 1. Enable MQTT broker.
	rec := doJSON(h.UpdateConfig, "PUT", "/projects/5/mqtt/config", `{"enabled":true,"allow_anonymous":false,"max_clients":50}`, proj)
	if rec.Code != http.StatusOK {
		t.Fatalf("update config: status %d body %s", rec.Code, rec.Body.String())
	}

	// 2. Add a device with auto-generated token and heartbeat watchdog.
	rec = doJSON(h.AddDevice, "POST", "/projects/5/mqtt/devices", `{"name":"esp_1","username":"esp1","allow_pub":"sensors/esp1/#","allow_sub":"actuators/esp1/#","heartbeat_sec":300}`, proj)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add device: status %d body %s", rec.Code, rec.Body.String())
	}
	var devResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &devResp); err != nil || !strings.HasPrefix(devResp.Token, "ofmqtt_") {
		t.Fatalf("expected generated ofmqtt_ token, got %s", rec.Body.String())
	}

	// 3. Add a LAN bridge (password encrypted at rest, never echoed in status).
	rec = doJSON(h.AddBridge, "POST", "/projects/5/mqtt/bridges", `{"name":"lan_ha","broker_url":"tcp://192.168.1.50:1883","remote_sub":"zigbee2mqtt/#","local_prefix":"lan/","password":"secret-lan-password"}`, proj)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add bridge: status %d body %s", rec.Code, rec.Body.String())
	}

	// 4. Status returns config, device (no secret_hash), and bridge (no password).
	rec = doJSON(h.Status, "GET", "/projects/5/mqtt", "", proj)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, devResp.Token) || strings.Contains(body, "secret-lan-password") {
		t.Fatalf("status must never leak device tokens or bridge passwords: %s", body)
	}
	if !strings.Contains(body, `"esp_1"`) || !strings.Contains(body, `"lan_ha"`) {
		t.Fatalf("expected device and bridge in status: %s", body)
	}

	// 5. Delete device and bridge.
	rec = doJSON(h.DeleteDevice, "DELETE", "/projects/5/mqtt/devices/esp_1", "", map[string]string{"projectID": "5", "name": "esp_1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete device: %d body %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(h.DeleteBridge, "DELETE", "/projects/5/mqtt/bridges/lan_ha", "", map[string]string{"projectID": "5", "name": "lan_ha"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete bridge: %d body %s", rec.Code, rec.Body.String())
	}
}
