package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsCarrierNAT(t *testing.T) {
	for ip, want := range map[string]bool{
		"100.64.0.1":     true,
		"100.78.241.44":  true,
		"100.127.255.1":  true,  // top of 100.64.0.0/10
		"100.128.0.1":    false, // just outside it
		"100.63.255.255": false,
		"192.168.1.20":   false, // private, but that is double NAT, not CGNAT
		"203.0.113.7":    false,
		"2001:db8::1":    false,
	} {
		if got := IsCarrierNAT(net.ParseIP(ip)); got != want {
			t.Errorf("IsCarrierNAT(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestParseProcNetRoute(t *testing.T) {
	const table = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	gw, err := parseProcNetRoute(table)
	if err != nil {
		t.Fatal(err)
	}
	if gw.String() != "192.168.1.1" {
		t.Errorf("gateway = %s, want 192.168.1.1", gw)
	}
	if _, err := parseProcNetRoute(strings.SplitAfterN(table, "\n", 3)[0] + "eth0\t0001A8C0\t00000000\t0001\n"); err == nil {
		t.Error("expected an error for a table without a default route")
	}
}

func TestNATPMPCodec(t *testing.T) {
	req := encodeNATPMPMap(443, 443, 7200)
	want := []byte{0, 2, 0, 0, 0x01, 0xbb, 0x01, 0xbb, 0, 0, 0x1c, 0x20}
	if string(req) != string(want) {
		t.Errorf("map request = % x, want % x", req, want)
	}

	ext := []byte{0, 128, 0, 0, 0, 0, 0, 9, 203, 0, 113, 7}
	ip, err := parseNATPMPExternal(ext)
	if err != nil || ip.String() != "203.0.113.7" {
		t.Errorf("external = %v, %v", ip, err)
	}
	ext[3] = 2
	if _, err := parseNATPMPExternal(ext); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("result 2 = %v, want ErrNotAuthorized", err)
	}

	resp := []byte{0, 130, 0, 0, 0, 0, 0, 9, 0x01, 0xbb, 0x01, 0xbb, 0, 0, 0x0e, 0x10}
	life, gotExt, err := parseNATPMPMap(resp, 443)
	if err != nil || life != 3600 || gotExt != 443 {
		t.Errorf("map response = %d, %d, %v", life, gotExt, err)
	}
	if _, _, err := parseNATPMPMap(resp, 80); err == nil {
		t.Error("expected an error for a response about another port")
	}
}

func TestPCPCodec(t *testing.T) {
	var nonce [12]byte
	copy(nonce[:], "abcdefghijkl")
	req := encodePCPMap(net.IPv4(192, 168, 1, 20), nonce, 80, 80, 7200)
	if len(req) != 60 || req[0] != 2 || req[1] != pcpOpMap {
		t.Fatalf("bad header: % x", req[:4])
	}
	if got := net.IP(req[8:24]).String(); got != "192.168.1.20" {
		t.Errorf("client address = %s", got)
	}
	if req[pcpHeaderLen+12] != pcpProtoTCP {
		t.Errorf("protocol = %d, want TCP", req[pcpHeaderLen+12])
	}

	// A router's answer: same payload, response bit set, lifetime and
	// external address filled in.
	resp := append([]byte(nil), req...)
	resp[1] |= 0x80
	resp[3] = 0
	binary.BigEndian.PutUint32(resp[4:], 3600)
	copy(resp[pcpHeaderLen+20:], net.IPv4(203, 0, 113, 7).To16())
	res, err := parsePCPMap(resp)
	if err != nil {
		t.Fatal(err)
	}
	if res.lifetime != 3600 || res.externalPort != 80 || res.externalIP.String() != "203.0.113.7" {
		t.Errorf("parsed = %+v", res)
	}
	resp[3] = 2
	if _, err := parsePCPMap(resp); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("result 2 = %v, want ErrNotAuthorized", err)
	}
}

// fakeNATPMP answers NAT-PMP on a loopback UDP port.
func fakeNATPMP(t *testing.T, grantExt func(req int) int) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			switch {
			case n == 2 && buf[1] == natpmpOpExternal:
				conn.WriteToUDP([]byte{0, 128, 0, 0, 0, 0, 0, 1, 203, 0, 113, 7}, from)
			case n == 12 && buf[1] == natpmpOpMapTCP:
				resp := make([]byte, 16)
				resp[1] = 130
				copy(resp[8:10], buf[4:6])
				ext := grantExt(int(binary.BigEndian.Uint16(buf[6:])))
				binary.BigEndian.PutUint16(resp[10:], uint16(ext))
				copy(resp[12:16], buf[8:12])
				conn.WriteToUDP(resp, from)
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

func TestNATPMPClientRoundTrip(t *testing.T) {
	addr := fakeNATPMP(t, func(req int) int { return req })
	c := &natpmpClient{gateway: addr}
	ctx := context.Background()
	ip, err := c.ExternalIP(ctx)
	if err != nil || ip.String() != "203.0.113.7" {
		t.Fatalf("ExternalIP = %v, %v", ip, err)
	}
	m, err := c.AddTCP(ctx, 443, 443, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if m.ExternalPort != 443 || m.Lifetime != 2*time.Hour || m.ExternalIP != "203.0.113.7" {
		t.Errorf("mapping = %+v", m)
	}
}

func TestNATPMPClientRejectsOtherExternalPort(t *testing.T) {
	addr := fakeNATPMP(t, func(req int) int {
		if req == 0 {
			return 0 // a delete
		}
		return 8443
	})
	c := &natpmpClient{gateway: addr}
	if _, err := c.AddTCP(context.Background(), 443, 443, time.Hour); !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestParseSSDPLocation(t *testing.T) {
	router := net.ParseIP("192.168.1.1")
	reply := func(loc string) []byte {
		return []byte("HTTP/1.1 200 OK\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nLOCATION: " + loc + "\r\n\r\n")
	}
	if loc, ok := parseSSDPLocation(reply("http://192.168.1.1:5000/rootDesc.xml"), router); !ok || loc != "http://192.168.1.1:5000/rootDesc.xml" {
		t.Errorf("valid reply rejected: %q %v", loc, ok)
	}
	for _, bad := range []string{
		"http://192.168.1.99:5000/rootDesc.xml", // another host than the responder
		"http://example.com/rootDesc.xml",       // a name, not the responder
		"https://192.168.1.1/rootDesc.xml",      // not plain http
		"file:///etc/passwd",
	} {
		if _, ok := parseSSDPLocation(reply(bad), router); ok {
			t.Errorf("accepted LOCATION %q", bad)
		}
	}
	if _, ok := parseSSDPLocation(reply("http://8.8.8.8/x.xml"), net.ParseIP("8.8.8.8")); ok {
		t.Error("accepted a public responder")
	}
}

const igdDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <device>
  <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <serviceList><service><serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType><controlURL>/l3f</controlURL></service></serviceList>
  <deviceList><device>
   <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
   <deviceList><device>
    <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
    <serviceList>
     <service><serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType><controlURL>/ppp</controlURL></service>
     <service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>ctl/IPConn</controlURL></service>
    </serviceList>
   </device></deviceList>
  </device></deviceList>
 </device>
</root>`

func TestFindWANService(t *testing.T) {
	ctl, st, err := findWANService([]byte(igdDescription), "http://192.168.1.1:5000/desc/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if st != "urn:schemas-upnp-org:service:WANIPConnection:1" {
		t.Errorf("service = %s, want WANIPConnection preferred over WANPPPConnection", st)
	}
	if ctl != "http://192.168.1.1:5000/desc/ctl/IPConn" {
		t.Errorf("control URL = %s", ctl)
	}
	if _, _, err := findWANService([]byte(`<root><device></device></root>`), "http://192.168.1.1/"); err == nil {
		t.Error("expected an error without a WAN service")
	}
}

func soapFault(code int) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>` +
		`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>` +
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>` + strconv.Itoa(code) + `</errorCode><errorDescription>desc</errorDescription></UPnPError>` +
		`</detail></s:Fault></s:Body></s:Envelope>`
}

func soapOK(action, inner string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:` + action + `Response xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">` + inner + `</u:` + action + `Response></s:Body></s:Envelope>`
}

// fakeIGD serves SOAP actions, recording each call's action and body.
type fakeIGD struct {
	mu      sync.Mutex
	calls   []string
	handler func(action, body string) (int, string)
}

func (f *fakeIGD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	action := r.Header.Get("SOAPAction")
	action = strings.Trim(action[strings.Index(action, "#")+1:], `"`)
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, action)
	f.mu.Unlock()
	code, resp := f.handler(action, string(body))
	w.WriteHeader(code)
	w.Write([]byte(resp))
}

func newTestUPnP(t *testing.T, f *fakeIGD) *upnpClient {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &upnpClient{
		http:        srv.Client(),
		routerIP:    net.IPv4(127, 0, 0, 1),
		controlURL:  srv.URL + "/ctl",
		serviceType: "urn:schemas-upnp-org:service:WANIPConnection:1",
	}
}

func TestUPnPPermanentLeaseFallback(t *testing.T) {
	var leases []string
	f := &fakeIGD{handler: func(action, body string) (int, string) {
		switch action {
		case "AddPortMapping":
			lease := soapValue([]byte(body), "NewLeaseDuration")
			leases = append(leases, lease)
			if lease != "0" {
				return 500, soapFault(upnpErrOnlyPermanentLeases)
			}
			return 200, soapOK(action, "")
		case "GetExternalIPAddress":
			return 200, soapOK(action, "<NewExternalIPAddress>203.0.113.7</NewExternalIPAddress>")
		}
		return 500, soapFault(401)
	}}
	m, err := newTestUPnP(t, f).AddTCP(context.Background(), 80, 80, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(leases, ",") != "7200,0" {
		t.Errorf("leases tried = %v, want 7200 then 0", leases)
	}
	if m.Lifetime != 0 || m.ExternalIP != "203.0.113.7" {
		t.Errorf("mapping = %+v", m)
	}
}

func TestUPnPConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		owner   string
		wantErr error
		wantDel bool
	}{
		{"another device", "Plex", ErrConflict, false},
		{"our stale forward", Description, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted := false
			f := &fakeIGD{handler: func(action, _ string) (int, string) {
				switch action {
				case "AddPortMapping":
					if !deleted {
						return 500, soapFault(upnpErrConflict)
					}
					return 200, soapOK(action, "")
				case "GetSpecificPortMappingEntry":
					return 200, soapOK(action, "<NewPortMappingDescription>"+tc.owner+"</NewPortMappingDescription>")
				case "DeletePortMapping":
					deleted = true
					return 200, soapOK(action, "")
				case "GetExternalIPAddress":
					return 200, soapOK(action, "<NewExternalIPAddress>203.0.113.7</NewExternalIPAddress>")
				}
				return 500, soapFault(401)
			}}
			_, err := newTestUPnP(t, f).AddTCP(context.Background(), 443, 443, time.Hour)
			if !errors.Is(err, tc.wantErr) && !(tc.wantErr == nil && err == nil) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if deleted != tc.wantDel {
				t.Errorf("deleted = %v, want %v", deleted, tc.wantDel)
			}
		})
	}
}

func TestUPnPNotAuthorized(t *testing.T) {
	f := &fakeIGD{handler: func(string, string) (int, string) { return 500, soapFault(upnpErrNotAuthorized) }}
	if _, err := newTestUPnP(t, f).AddTCP(context.Background(), 80, 80, time.Hour); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("err = %v, want ErrNotAuthorized", err)
	}
}

// fakeClient records mappings in memory.
type fakeClient struct {
	mu       sync.Mutex
	mapped   map[int]bool
	failPort int
	lifetime time.Duration
	adds     int
}

func (c *fakeClient) Protocol() string { return "fake" }
func (c *fakeClient) ExternalIP(context.Context) (net.IP, error) {
	return net.IPv4(203, 0, 113, 7), nil
}
func (c *fakeClient) AddTCP(_ context.Context, ext, in int, _ time.Duration) (Mapping, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adds++
	if ext == c.failPort {
		return Mapping{}, ErrConflict
	}
	c.mapped[ext] = true
	return Mapping{Protocol: "fake", ExternalPort: ext, InternalPort: in, ExternalIP: "203.0.113.7", InternalIP: "192.168.1.20", Lifetime: c.lifetime}, nil
}
func (c *fakeClient) DeleteTCP(_ context.Context, ext, _ int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.mapped, ext)
	return nil
}

func newTestMapper(c *fakeClient) *Mapper {
	m := NewMapper(nil)
	m.discover = func(context.Context) (Client, Router, error) { return c, Router{Protocol: "fake"}, nil }
	return m
}

func TestMapperEnableDisable(t *testing.T) {
	c := &fakeClient{mapped: map[int]bool{}, lifetime: 2 * time.Hour}
	m := newTestMapper(c)
	st, err := m.Enable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || !st.Active || st.ExternalIP != "203.0.113.7" || st.InternalIP != "192.168.1.20" {
		t.Errorf("status = %+v", st)
	}
	if !c.mapped[80] || !c.mapped[443] {
		t.Errorf("mapped = %v, want 80 and 443", c.mapped)
	}
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.mapped) != 0 {
		t.Errorf("mapped after disable = %v", c.mapped)
	}
	if st := m.Status(); st.Enabled || st.Active {
		t.Errorf("status after disable = %+v", st)
	}
}

func TestMapperRollsBackPartialFailure(t *testing.T) {
	c := &fakeClient{mapped: map[int]bool{}, failPort: 443}
	m := newTestMapper(c)
	st, err := m.Enable(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if len(c.mapped) != 0 {
		t.Errorf("port 80 left mapped after 443 failed: %v", c.mapped)
	}
	if st.Enabled || st.Error == "" {
		t.Errorf("status = %+v, want disabled with an error", st)
	}
}

func TestMapperRenews(t *testing.T) {
	c := &fakeClient{mapped: map[int]bool{}, lifetime: 40 * time.Millisecond}
	m := newTestMapper(c)
	if _, err := m.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		adds := c.adds
		c.mu.Unlock()
		if adds >= 4 { // the initial pair plus at least one renewal pair
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("mappings were not renewed at half their lifetime")
}
