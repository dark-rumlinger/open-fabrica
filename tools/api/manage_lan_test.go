package api

import (
	"net"
	"strconv"
	"testing"

	"openfabrica/tools/tooltest"
)

func TestManageLAN_WakeAndPing(t *testing.T) {
	h := tooltest.New(t)

	// Wake-on-LAN to loopback succeeds; public IP broadcast is rejected.
	h.MustOK("manage_lan", map[string]any{
		"action":    "wake",
		"mac":       "AA:BB:CC:DD:EE:FF",
		"broadcast": "127.0.0.1",
		"port":      9,
	})
	h.MustFail("manage_lan", map[string]any{
		"action":    "wake",
		"mac":       "AA:BB:CC:DD:EE:FF",
		"broadcast": "8.8.8.8",
	}, "")

	// TCP ping against a real local listener succeeds; public internet & metadata IPs are rejected.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	res := h.MustOK("manage_lan", map[string]any{
		"action":     "ping",
		"host":       "127.0.0.1",
		"port":       port,
		"timeout_ms": 1000,
	})
	data := res.Data.(map[string]any)
	if ok, _ := data["reachable"].(bool); !ok {
		t.Fatalf("expected reachable=true, got %+v", data)
	}

	h.MustFail("manage_lan", map[string]any{
		"action": "ping",
		"host":   "169.254.169.254",
		"port":   80,
	}, "")
	h.MustFail("manage_lan", map[string]any{
		"action": "ping",
		"host":   "8.8.8.8",
		"port":   53,
	}, "")
}
