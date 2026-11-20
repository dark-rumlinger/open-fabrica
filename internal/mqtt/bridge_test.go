package mqtt

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestValidateBridgeURL(t *testing.T) {
	valid := []struct {
		raw      string
		wantAddr string
		wantTLS  bool
	}{
		{"tcp://192.168.1.50:1883", "192.168.1.50:1883", false},
		{"mqtt://mosquitto.local", "mosquitto.local:1883", false},
		{"tls://broker.example.com", "broker.example.com:8883", true},
		{"mqtts://10.0.0.5:8884", "10.0.0.5:8884", true},
		{"ssl://localhost:8883", "localhost:8883", true},
	}
	for _, tc := range valid {
		addr, _, useTLS, err := ValidateBridgeURL(tc.raw)
		if err != nil {
			t.Errorf("ValidateBridgeURL(%q) unexpected error: %v", tc.raw, err)
			continue
		}
		if addr != tc.wantAddr || useTLS != tc.wantTLS {
			t.Errorf("ValidateBridgeURL(%q) = (%q, %v), want (%q, %v)", tc.raw, addr, useTLS, tc.wantAddr, tc.wantTLS)
		}
	}

	invalid := []string{
		"",
		"http://192.168.1.50:1883",
		"tcp://",
		"tcp://169.254.169.254:1883",
		"tcp://metadata.google.internal:1883",
		"mqtt://169.254.170.2:1883",
	}
	for _, raw := range invalid {
		if _, _, _, err := ValidateBridgeURL(raw); err == nil {
			t.Errorf("ValidateBridgeURL(%q) expected error, got nil", raw)
		}
	}
}

func TestBridge_BidirectionalAndLoopPrevention(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start a real "remote" LAN broker on an ephemeral TCP port.
	var remoteMu sync.Mutex
	var remoteEvents []PublishEvent
	remoteBroker := NewBroker(BrokerConfig{
		Auth: func(clientID, username, password string) (AuthResult, bool) {
			if username == "bridge_user" && password == "bridge_pass" {
				return AuthResult{DeviceName: clientID, Username: username, AllowPub: "#", AllowSub: "#"}, true
			}
			return AuthResult{}, false
		},
		OnPublish: func(ev PublishEvent) {
			remoteMu.Lock()
			remoteEvents = append(remoteEvents, ev)
			remoteMu.Unlock()
		},
	})
	defer remoteBroker.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		_ = ServeTCP(ctx, ln, func(cctx context.Context, conn net.Conn) {
			remoteBroker.HandleConn(cctx, conn)
		})
	}()

	// 2. Start the "local" project broker with an outbound bridge to the remote broker.
	// Deliberately overlap RemoteSub and LocalPub (both cover zigbee2mqtt/#) to prove
	// OriginBridge loop prevention stops infinite echoing.
	var localMu sync.Mutex
	var localEvents []PublishEvent
	localBroker := NewBroker(BrokerConfig{
		OnPublish: func(ev PublishEvent) {
			localMu.Lock()
			localEvents = append(localEvents, ev)
			localMu.Unlock()
		},
		Bridges: []BridgeConfig{
			{
				Name:        "lan_mosquitto",
				BrokerURL:   "tcp://" + ln.Addr().String(),
				ClientID:    "of-test-bridge-1",
				Username:    "bridge_user",
				Password:    "bridge_pass",
				RemoteSub:   []string{"zigbee2mqtt/#"},
				LocalPrefix: "lan/",
				LocalPub:    []string{"lan/zigbee2mqtt/#"},
				QoS:         1,
			},
		},
	})
	defer localBroker.Close()

	// Wait until bridge connects.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := localBroker.Stats()
		if len(st.Bridges) == 1 && st.Bridges[0].Connected {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if st := localBroker.Stats(); len(st.Bridges) != 1 || !st.Bridges[0].Connected {
		t.Fatalf("bridge did not connect: %+v", st.Bridges)
	}

	// 3. Remote -> Local: publish on remote zigbee2mqtt/lamp, expect local lan/zigbee2mqtt/lamp.
	if err := remoteBroker.Publish("zigbee2mqtt/lamp", []byte(`{"state":"ON"}`), 1, false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	gotInbound := false
	for time.Now().Before(deadline) {
		localMu.Lock()
		for _, ev := range localEvents {
			if ev.Topic == "lan/zigbee2mqtt/lamp" && string(ev.Payload) == `{"state":"ON"}` && ev.OriginBridge == "lan_mosquitto" {
				gotInbound = true
				break
			}
		}
		localMu.Unlock()
		if gotInbound {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !gotInbound {
		t.Fatalf("did not receive bridged inbound message; localEvents=%+v", localEvents)
	}

	// Give a brief window and verify the inbound message was NOT echoed back to remoteBroker.
	time.Sleep(100 * time.Millisecond)
	remoteMu.Lock()
	lampCount := 0
	for _, ev := range remoteEvents {
		if ev.Topic == "zigbee2mqtt/lamp" {
			lampCount++
		}
	}
	remoteMu.Unlock()
	if lampCount != 1 {
		t.Fatalf("expected exactly 1 remote publish on zigbee2mqtt/lamp (no loop echo), got %d", lampCount)
	}

	// 4. Local -> Remote: publish command locally on lan/zigbee2mqtt/lamp/set, expect remote zigbee2mqtt/lamp/set.
	if err := localBroker.Publish("lan/zigbee2mqtt/lamp/set", []byte(`{"state":"OFF"}`), 1, false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	gotOutbound := false
	for time.Now().Before(deadline) {
		remoteMu.Lock()
		for _, ev := range remoteEvents {
			if ev.Topic == "zigbee2mqtt/lamp/set" && string(ev.Payload) == `{"state":"OFF"}` {
				gotOutbound = true
				break
			}
		}
		remoteMu.Unlock()
		if gotOutbound {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !gotOutbound {
		t.Fatalf("did not receive bridged outbound command on remote broker; remoteEvents=%+v", remoteEvents)
	}

	st := localBroker.Stats()
	if st.Bridges[0].MessagesIn < 1 || st.Bridges[0].MessagesOut < 1 {
		t.Fatalf("expected MessagesIn >= 1 and MessagesOut >= 1, got %+v", st.Bridges[0])
	}
}
