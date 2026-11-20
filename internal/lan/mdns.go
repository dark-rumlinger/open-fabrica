package lan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sort"
	"strings"
	"time"
)

const (
	mdnsMulticastAddr  = "224.0.0.251:5353"
	defaultMDNSTimeout = 1500 * time.Millisecond
	minMDNSTimeout     = 200 * time.Millisecond
	maxMDNSTimeout     = 4000 * time.Millisecond

	dnsTypeA   uint16 = 1
	dnsTypePTR uint16 = 12
	dnsTypeTXT uint16 = 16
	dnsTypeSRV uint16 = 33
)

// DiscoveredService represents one LAN service discovered via mDNS/Bonjour.
type DiscoveredService struct {
	Name    string            `json:"name"`
	Service string            `json:"service"`
	Host    string            `json:"host,omitempty"`
	IP      string            `json:"ip,omitempty"`
	Port    int               `json:"port,omitempty"`
	TXT     map[string]string `json:"txt,omitempty"`
}

// Discover browses the local network via mDNS (RFC 6762) for the given service
// type (e.g. "_mqtt._tcp.local.", "_http._tcp.local.", "_esphomelib._tcp.local.",
// "_home-assistant._tcp.local.", "_smb._tcp.local.", or "_services._dns-sd._udp.local.").
func Discover(ctx context.Context, service string, timeout time.Duration) ([]DiscoveredService, error) {
	svc := normalizeMDNSService(service)
	if timeout <= 0 {
		timeout = defaultMDNSTimeout
	}
	if timeout < minMDNSTimeout {
		timeout = minMDNSTimeout
	}
	if timeout > maxMDNSTimeout {
		timeout = maxMDNSTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}

	query := encodeMDNSQuery(svc)
	dst, err := net.ResolveUDPAddr("udp4", mdnsMulticastAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.WriteToUDP(query, dst); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetReadDeadline(deadline)
	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-doneCh:
		}
	}()

	instances := make(map[string]*DiscoveredService)
	hostIPs := make(map[string]string)
	buf := make([]byte, 8192)

	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			break
		}
		parseMDNSResponse(buf[:n], svc, src.IP.String(), instances, hostIPs)
	}

	out := make([]DiscoveredService, 0, len(instances))
	for _, inst := range instances {
		if inst.IP == "" && inst.Host != "" {
			if ip, ok := hostIPs[strings.ToLower(inst.Host)]; ok {
				inst.IP = ip
			}
		}
		out = append(out, *inst)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Host < out[j].Host
	})
	return out, nil
}

func normalizeMDNSService(service string) string {
	s := strings.TrimSpace(service)
	if s == "" {
		s = "_http._tcp.local."
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	if !strings.HasSuffix(strings.ToLower(s), ".local.") {
		s += "local."
	}
	return s
}

func encodeMDNSQuery(qname string) []byte {
	var b bytes.Buffer
	// 12-byte DNS header: ID=0, Flags=0, QDCOUNT=1, AN/NS/AR=0.
	b.Write([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	for _, label := range strings.Split(strings.TrimSuffix(qname, "."), ".") {
		if label == "" || len(label) > 63 {
			continue
		}
		b.WriteByte(byte(len(label)))
		b.WriteString(label)
	}
	b.WriteByte(0)
	// QTYPE = PTR (12), QCLASS = IN (1) with unicast-response bit (0x8001) so
	// responders reply directly to our ephemeral UDP port as well as multicast.
	var tail [4]byte
	binary.BigEndian.PutUint16(tail[0:2], dnsTypePTR)
	binary.BigEndian.PutUint16(tail[2:4], 0x8001)
	b.Write(tail[:])
	return b.Bytes()
}

func parseMDNSResponse(pkt []byte, querySvc, srcIP string, instances map[string]*DiscoveredService, hostIPs map[string]string) {
	if len(pkt) < 12 {
		return
	}
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))
	anCount := int(binary.BigEndian.Uint16(pkt[6:8]))
	nsCount := int(binary.BigEndian.Uint16(pkt[8:10]))
	arCount := int(binary.BigEndian.Uint16(pkt[10:12]))

	off := 12
	for range qdCount {
		_, next, err := readDNSName(pkt, off)
		if err != nil || next+4 > len(pkt) {
			return
		}
		off = next + 4
	}

	totalRR := anCount + nsCount + arCount
	for range totalRR {
		name, next, err := readDNSName(pkt, off)
		if err != nil || next+10 > len(pkt) {
			return
		}
		rrType := binary.BigEndian.Uint16(pkt[next : next+2])
		rdLen := int(binary.BigEndian.Uint16(pkt[next+8 : next+10]))
		rdataOff := next + 10
		if rdataOff+rdLen > len(pkt) {
			return
		}
		rdata := pkt[rdataOff : rdataOff+rdLen]
		off = rdataOff + rdLen

		switch rrType {
		case dnsTypePTR:
			target, _, err := readDNSName(pkt, rdataOff)
			if err == nil && target != "" {
				key := strings.ToLower(target)
				if instances[key] == nil {
					instances[key] = &DiscoveredService{
						Name:    trimServiceSuffix(target, name),
						Service: strings.TrimSuffix(name, "."),
						IP:      srcIP,
					}
				}
			}
		case dnsTypeSRV:
			if len(rdata) >= 6 {
				port := int(binary.BigEndian.Uint16(rdata[4:6]))
				targetHost, _, err := readDNSName(pkt, rdataOff+6)
				if err == nil {
					key := strings.ToLower(name)
					inst := instances[key]
					if inst == nil {
						inst = &DiscoveredService{
							Name:    trimServiceSuffix(name, querySvc),
							Service: strings.TrimSuffix(querySvc, "."),
						}
						instances[key] = inst
					}
					inst.Host = strings.TrimSuffix(targetHost, ".")
					inst.Port = port
					if inst.IP == "" {
						inst.IP = srcIP
					}
				}
			}
		case dnsTypeTXT:
			key := strings.ToLower(name)
			if inst := instances[key]; inst != nil {
				txt := parseDNSTXT(rdata)
				if len(txt) > 0 {
					inst.TXT = txt
				}
			}
		case dnsTypeA:
			if len(rdata) == 4 {
				ip := net.IPv4(rdata[0], rdata[1], rdata[2], rdata[3]).String()
				hostIPs[strings.ToLower(strings.TrimSuffix(name, "."))] = ip
			}
		}
	}
}

func trimServiceSuffix(full, svc string) string {
	f := strings.TrimSuffix(full, ".")
	s := "." + strings.TrimSuffix(svc, ".")
	if strings.HasSuffix(strings.ToLower(f), strings.ToLower(s)) {
		return f[:len(f)-len(s)]
	}
	return f
}

func parseDNSTXT(rdata []byte) map[string]string {
	out := make(map[string]string)
	for i := 0; i < len(rdata); {
		n := int(rdata[i])
		i++
		if n == 0 || i+n > len(rdata) {
			break
		}
		entry := string(rdata[i : i+n])
		i += n
		if k, v, ok := strings.Cut(entry, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

func readDNSName(pkt []byte, off int) (string, int, error) {
	var labels []string
	cur := off
	nextAfter := -1
	jumped := 0
	for {
		if cur >= len(pkt) {
			return "", 0, errors.New("dns name truncated")
		}
		length := int(pkt[cur])
		if length == 0 {
			if nextAfter == -1 {
				nextAfter = cur + 1
			}
			break
		}
		if length&0xc0 == 0xc0 {
			if cur+1 >= len(pkt) {
				return "", 0, errors.New("dns pointer truncated")
			}
			ptr := int(binary.BigEndian.Uint16(pkt[cur:cur+2]) & 0x3fff)
			if nextAfter == -1 {
				nextAfter = cur + 2
			}
			jumped++
			if jumped > 16 || ptr >= len(pkt) {
				return "", 0, errors.New("dns pointer loop")
			}
			cur = ptr
			continue
		}
		cur++
		if cur+length > len(pkt) {
			return "", 0, errors.New("dns label out of bounds")
		}
		labels = append(labels, string(pkt[cur:cur+length]))
		cur += length
	}
	return strings.Join(labels, ".") + ".", nextAfter, nil
}
