package lan

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestBuildMagicPacket(t *testing.T) {
	hw, err := ParseMAC("AA:BB:CC:DD:EE:FF")
	if err != nil {
		t.Fatal(err)
	}
	pkt := BuildMagicPacket(hw)
	if len(pkt) != 102 {
		t.Fatalf("expected 102-byte magic packet, got %d", len(pkt))
	}
	if !bytes.Equal(pkt[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Fatal("missing 6x 0xFF sync header")
	}
	for i := range 16 {
		off := 6 + i*6
		if !bytes.Equal(pkt[off:off+6], hw) {
			t.Fatalf("repetition %d mismatch", i)
		}
	}
}

func TestWakeValidation(t *testing.T) {
	if _, _, err := Wake("not-a-mac", "", 9); err == nil {
		t.Fatal("expected error for invalid MAC")
	}
	if _, _, err := Wake("AA:BB:CC:DD:EE:FF", "8.8.8.8", 9); err == nil {
		t.Fatal("expected error for public broadcast IP")
	}
	// Sending to loopback UDP port succeeds cleanly without touching the LAN.
	if bcast, port, err := Wake("AA:BB:CC:DD:EE:FF", "127.0.0.1", 9); err != nil || bcast != "127.0.0.1" || port != 9 {
		t.Fatalf("expected loopback wake ok, got %q %d %v", bcast, port, err)
	}
}

func TestValidateLANHostAndTCPPing(t *testing.T) {
	for _, okHost := range []string{"192.168.1.50", "10.0.0.1", "127.0.0.1", "localhost", "mosquitto.local", "nas"} {
		if err := ValidateLANHost(okHost); err != nil {
			t.Errorf("ValidateLANHost(%q) unexpected error: %v", okHost, err)
		}
	}
	for _, badHost := range []string{"", "8.8.8.8", "1.1.1.1", "169.254.169.254", "metadata.google.internal", "example.com"} {
		if err := ValidateLANHost(badHost); err == nil {
			t.Errorf("ValidateLANHost(%q) expected error, got nil", badHost)
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ok, _, err := TCPPing(context.Background(), "127.0.0.1", port, time.Second)
	if err != nil || !ok {
		t.Fatalf("expected reachable 127.0.0.1:%d, got ok=%v err=%v", port, ok, err)
	}
}

func TestParseMDNSResponse(t *testing.T) {
	// Synthesize a minimal DNS response with PTR + SRV + A records for _mqtt._tcp.local.
	var pkt bytes.Buffer
	// Header: ANCOUNT=3
	pkt.Write([]byte{0, 0, 0x84, 0x00, 0, 0, 0, 3, 0, 0, 0, 0})
	writeName := func(s string) {
		for _, lbl := range bytes.Split([]byte(s), []byte(".")) {
			if len(lbl) == 0 {
				continue
			}
			pkt.WriteByte(byte(len(lbl)))
			pkt.Write(lbl)
		}
		pkt.WriteByte(0)
	}
	writeRRHeader := func(name string, rrType uint16, rdlen uint16) {
		writeName(name)
		var h [10]byte
		binary.BigEndian.PutUint16(h[0:2], rrType)
		binary.BigEndian.PutUint16(h[2:4], 1)
		binary.BigEndian.PutUint32(h[4:8], 120)
		binary.BigEndian.PutUint16(h[8:10], rdlen)
		pkt.Write(h[:])
	}

	// 1. PTR: _mqtt._tcp.local -> mosquitto._mqtt._tcp.local
	var ptrBuf bytes.Buffer
	for _, lbl := range []string{"mosquitto", "_mqtt", "_tcp", "local"} {
		ptrBuf.WriteByte(byte(len(lbl)))
		ptrBuf.WriteString(lbl)
	}
	ptrBuf.WriteByte(0)
	writeRRHeader("_mqtt._tcp.local", dnsTypePTR, uint16(ptrBuf.Len()))
	pkt.Write(ptrBuf.Bytes())

	// 2. SRV: mosquitto._mqtt._tcp.local -> pi.local:1883
	var srvBuf bytes.Buffer
	srvBuf.Write([]byte{0, 0, 0, 0, 0x07, 0x5b}) // port 1883
	for _, lbl := range []string{"pi", "local"} {
		srvBuf.WriteByte(byte(len(lbl)))
		srvBuf.WriteString(lbl)
	}
	srvBuf.WriteByte(0)
	writeRRHeader("mosquitto._mqtt._tcp.local", dnsTypeSRV, uint16(srvBuf.Len()))
	pkt.Write(srvBuf.Bytes())

	// 3. A: pi.local -> 192.168.1.50
	writeRRHeader("pi.local", dnsTypeA, 4)
	pkt.Write([]byte{192, 168, 1, 50})

	instances := make(map[string]*DiscoveredService)
	hostIPs := make(map[string]string)
	parseMDNSResponse(pkt.Bytes(), "_mqtt._tcp.local.", "192.168.1.50", instances, hostIPs)
	if len(instances) != 1 {
		t.Fatalf("expected 1 discovered instance, got %+v", instances)
	}
	for _, inst := range instances {
		if inst.Name != "mosquitto" || inst.Host != "pi.local" || inst.Port != 1883 || hostIPs["pi.local"] != "192.168.1.50" {
			t.Fatalf("unexpected parsed service: %+v hostIPs=%v", inst, hostIPs)
		}
	}
}
