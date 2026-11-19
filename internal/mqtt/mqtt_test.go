package mqtt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

func buildConnectBody(clientID, user, pass string, clean bool, keepAlive uint16, willTopic string, willMsg []byte) []byte {
	var b bytes.Buffer
	writeUTF8(&b, "MQTT")
	b.WriteByte(4)
	var flags byte
	if clean {
		flags |= 0x02
	}
	if willTopic != "" {
		flags |= 0x04 | 0x08 | 0x20 // QoS 1 + retain
	}
	if user != "" {
		flags |= 0x80
	}
	if pass != "" {
		flags |= 0x40
	}
	b.WriteByte(flags)
	var ka [2]byte
	binary.BigEndian.PutUint16(ka[:], keepAlive)
	b.Write(ka[:])
	writeUTF8(&b, clientID)
	if willTopic != "" {
		writeUTF8(&b, willTopic)
		var wl [2]byte
		binary.BigEndian.PutUint16(wl[:], uint16(len(willMsg)))
		b.Write(wl[:])
		b.Write(willMsg)
	}
	if user != "" {
		writeUTF8(&b, user)
	}
	if pass != "" {
		var pl [2]byte
		binary.BigEndian.PutUint16(pl[:], uint16(len(pass)))
		b.Write(pl[:])
		b.WriteString(pass)
	}
	return b.Bytes()
}

func buildSubscribeBody(pktID uint16, filter string, qos byte) []byte {
	var b bytes.Buffer
	var id [2]byte
	binary.BigEndian.PutUint16(id[:], pktID)
	b.Write(id[:])
	writeUTF8(&b, filter)
	b.WriteByte(qos)
	return b.Bytes()
}

func TestTopicMatching(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"sensors/+/temp", "sensors/esp1/temp", true},
		{"sensors/+/temp", "sensors/esp1/hum", false},
		{"sensors/#", "sensors/esp1/temp", true},
		{"#", "a/b/c", true},
		{"#", "$SYS/broker/clients", false},
		{"+/clients", "$SYS/clients", false},
		{"$SYS/#", "$SYS/broker/clients", true},
	}
	for _, tc := range cases {
		if got := MatchTopic(tc.filter, tc.topic); got != tc.want {
			t.Errorf("MatchTopic(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}

func TestBroker_PubSubRetainedAndLWT(t *testing.T) {
	var mu sync.Mutex
	var events []PublishEvent
	b := NewBroker(BrokerConfig{
		OnPublish: func(ev PublishEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscriber 1 connects and subscribes to sensors/#
	s1Client, s1Server := net.Pipe()
	defer s1Client.Close()
	go b.HandleConn(ctx, s1Server)

	if err := writePacket(s1Client, pktConnect, 0, buildConnectBody("sub-1", "", "", true, 30, "", nil)); err != nil {
		t.Fatal(err)
	}
	r1 := bufio.NewReader(s1Client)
	ack, err := readPacket(r1)
	if err != nil || ack.ptype != pktConnAck || ack.body[1] != connAccepted {
		t.Fatalf("sub1 connack failed: %+v %v", ack, err)
	}
	if err := writePacket(s1Client, pktSubscribe, 0x02, buildSubscribeBody(1, "sensors/#", 1)); err != nil {
		t.Fatal(err)
	}
	subAck, err := readPacket(r1)
	if err != nil || subAck.ptype != pktSubAck || subAck.body[2] != 1 {
		t.Fatalf("sub1 suback failed: %+v %v", subAck, err)
	}

	// Publisher connects with LWT, sends QoS 1 retained message, then drops abruptly.
	pClient, pServer := net.Pipe()
	go b.HandleConn(ctx, pServer)
	if err := writePacket(pClient, pktConnect, 0, buildConnectBody("esp-1", "", "", true, 30, "sensors/esp1/status", []byte("offline"))); err != nil {
		t.Fatal(err)
	}
	pr := bufio.NewReader(pClient)
	if pAck, err := readPacket(pr); err != nil || pAck.ptype != pktConnAck {
		t.Fatalf("pub connack failed: %v", err)
	}

	pubBody, pubFlags := encodePublish("sensors/esp1/temp", []byte(`{"c":22.5}`), 1, true, 7)
	if err := writePacket(pClient, pktPublish, pubFlags, pubBody); err != nil {
		t.Fatal(err)
	}
	pubAck, err := readPacket(pr)
	if err != nil || pubAck.ptype != pktPubAck {
		t.Fatalf("expected PUBACK, got %+v %v", pubAck, err)
	}

	// Subscriber 1 receives the live publish.
	msg1, err := readPacket(r1)
	if err != nil || msg1.ptype != pktPublish {
		t.Fatalf("sub1 expected publish, got %+v %v", msg1, err)
	}
	parsed1, err := parsePublish(msg1.flags, msg1.body)
	if err != nil || parsed1.topic != "sensors/esp1/temp" || string(parsed1.payload) != `{"c":22.5}` {
		t.Fatalf("unexpected msg1: %+v %v", parsed1, err)
	}

	// Abruptly close publisher -> triggers LWT on sensors/esp1/status!
	_ = pClient.Close()
	msgLWT, err := readPacket(r1)
	if err != nil || msgLWT.ptype != pktPublish {
		t.Fatalf("sub1 expected LWT publish, got %+v %v", msgLWT, err)
	}
	parsedLWT, err := parsePublish(msgLWT.flags, msgLWT.body)
	if err != nil || parsedLWT.topic != "sensors/esp1/status" || string(parsedLWT.payload) != "offline" {
		t.Fatalf("unexpected LWT: %+v %v", parsedLWT, err)
	}

	// Late Subscriber 2 connects -> receives retained temp & retained LWT!
	s2Client, s2Server := net.Pipe()
	defer s2Client.Close()
	go b.HandleConn(ctx, s2Server)
	_ = writePacket(s2Client, pktConnect, 0, buildConnectBody("sub-2", "", "", true, 30, "", nil))
	r2 := bufio.NewReader(s2Client)
	_, _ = readPacket(r2)
	_ = writePacket(s2Client, pktSubscribe, 0x02, buildSubscribeBody(2, "sensors/#", 0))
	_, _ = readPacket(r2) // SUBACK

	gotTopics := map[string]bool{}
	for range 2 {
		rm, err := readPacket(r2)
		if err != nil {
			t.Fatal(err)
		}
		pm, err := parsePublish(rm.flags, rm.body)
		if err != nil {
			t.Fatal(err)
		}
		gotTopics[pm.topic] = true
	}
	if !gotTopics["sensors/esp1/temp"] || !gotTopics["sensors/esp1/status"] {
		t.Fatalf("expected both retained messages on late subscribe, got %v", gotTopics)
	}

	// Verify OnPublish hook received events.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 {
		t.Fatalf("expected >= 2 publish hook events, got %d", len(events))
	}
}

func TestBroker_ACLEnforcement(t *testing.T) {
	b := NewBroker(BrokerConfig{
		Auth: func(clientID, username, password string) (AuthResult, bool) {
			if username == "sensor1" && password == "secret" {
				return AuthResult{
					DeviceName: "sensor1",
					Username:   "sensor1",
					AllowPub:   "devices/sensor1/#",
					AllowSub:   "commands/sensor1/#",
				}, true
			}
			return AuthResult{}, false
		},
	})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bad credentials -> connNotAuthorized
	badC, badS := net.Pipe()
	go b.HandleConn(ctx, badS)
	_ = writePacket(badC, pktConnect, 0, buildConnectBody("x", "sensor1", "wrong", true, 30, "", nil))
	ack, err := readPacket(bufio.NewReader(badC))
	if err != nil || ack.ptype != pktConnAck || ack.body[1] != connNotAuthorized {
		t.Fatalf("expected connNotAuthorized, got %+v %v", ack, err)
	}
	_ = badC.Close()

	// Valid credentials -> unauthorized subscribe returns 0x80
	c, s := net.Pipe()
	defer c.Close()
	go b.HandleConn(ctx, s)
	_ = writePacket(c, pktConnect, 0, buildConnectBody("sensor1", "sensor1", "secret", true, 30, "", nil))
	r := bufio.NewReader(c)
	_, _ = readPacket(r)
	_ = writePacket(c, pktSubscribe, 0x02, buildSubscribeBody(1, "devices/#", 0))
	subAck, err := readPacket(r)
	if err != nil || subAck.ptype != pktSubAck || subAck.body[2] != 0x80 {
		t.Fatalf("expected 0x80 subscribe failure for disallowed topic, got %+v %v", subAck, err)
	}
}
