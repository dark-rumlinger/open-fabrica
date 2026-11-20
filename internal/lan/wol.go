// Package lan implements pure-Go, zero-dependency local-network superpowers
// for Open Fabrica: Wake-on-LAN magic packets, LAN-scoped TCP reachability
// checks, and mDNS/Bonjour service discovery (RFC 6762).
package lan

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
)

const (
	defaultWoLPort      = 9
	defaultWoLBroadcast = "255.255.255.255"
)

// ParseMAC parses a standard 6-byte Ethernet MAC address (AA:BB:CC:DD:EE:FF or AA-BB-CC-DD-EE-FF).
func ParseMAC(raw string) (net.HardwareAddr, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("mac address is required")
	}
	hw, err := net.ParseMAC(s)
	if err != nil {
		return nil, fmt.Errorf("invalid mac address %q: %w", raw, err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("mac address %q must be 6 bytes (EUI-48)", raw)
	}
	return hw, nil
}

// BuildMagicPacket constructs the standard 102-byte IEEE 802.3 Wake-on-LAN
// magic packet (6 bytes of 0xFF followed by 16 repetitions of the 6-byte MAC).
func BuildMagicPacket(mac net.HardwareAddr) []byte {
	var buf bytes.Buffer
	buf.Grow(102)
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	for range 16 {
		buf.Write(mac)
	}
	return buf.Bytes()
}

// Wake sends a Wake-on-LAN UDP magic packet to the given MAC address.
// broadcastAddr defaults to 255.255.255.255 and must be either the limited
// broadcast address or a private RFC 1918 IPv4 address (e.g. 192.168.1.255).
// port defaults to 9 (UDP discard/WoL).
func Wake(mac, broadcastAddr string, port int) (string, int, error) {
	hw, err := ParseMAC(mac)
	if err != nil {
		return "", 0, err
	}
	bcast := strings.TrimSpace(broadcastAddr)
	if bcast == "" {
		bcast = defaultWoLBroadcast
	}
	ip := net.ParseIP(bcast)
	if ip == nil || ip.To4() == nil {
		return "", 0, fmt.Errorf("broadcast address %q must be an IPv4 address", bcast)
	}
	if !ip.Equal(net.IPv4bcast) && !ip.IsPrivate() && !ip.IsLoopback() {
		return "", 0, fmt.Errorf("broadcast address %q must be 255.255.255.255 or a private LAN IPv4 address", bcast)
	}
	if port <= 0 {
		port = defaultWoLPort
	}
	if port > 65535 {
		return "", 0, fmt.Errorf("invalid udp port %d", port)
	}

	pkt := BuildMagicPacket(hw)
	dst := &net.UDPAddr{IP: ip.To4(), Port: port}
	conn, err := net.DialUDP("udp4", nil, dst)
	if err != nil {
		return "", 0, fmt.Errorf("dial wol udp %s: %w", dst.String(), err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(pkt); err != nil {
		return "", 0, fmt.Errorf("send wol packet: %w", err)
	}
	return bcast, port, nil
}
