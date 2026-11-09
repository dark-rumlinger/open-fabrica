package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UPnP Internet Gateway Device: an SSDP multicast search finds the router, its
// XML description names the WAN connection service, and SOAP calls against
// that service read the external address and manage port mappings.

var ssdpAddr = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

var ssdpTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

// wanServiceTypes are the services that carry port mappings, most capable first.
var wanServiceTypes = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// UPnP error codes this package acts on.
const (
	upnpErrNotAuthorized       = 606
	upnpErrConflict            = 718
	upnpErrOnlyPermanentLeases = 725
)

// maxDescriptionBytes caps what we read from a device on the LAN. Real IGD
// descriptions are a few kilobytes.
const maxDescriptionBytes = 256 << 10

type upnpClient struct {
	http        *http.Client
	routerIP    net.IP
	controlURL  string
	serviceType string
}

// SOAPError is a UPnP fault returned by the router.
type SOAPError struct {
	Code        int
	Description string
}

func (e *SOAPError) Error() string {
	return fmt.Sprintf("UPnP error %d: %s", e.Code, e.Description)
}

func probeUPnP(ctx context.Context) (Client, net.IP, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()

	for _, st := range ssdpTargets {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 1\r\n" +
			"ST: " + st + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), ssdpAddr); err != nil {
			return nil, nil, err
		}
	}
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(d)
	}

	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, nil, ErrNoRouter
		}
		location, ok := parseSSDPLocation(buf[:n], from.IP)
		if !ok || seen[location] {
			continue
		}
		seen[location] = true
		c, err := newUPnPClient(ctx, location, from.IP)
		if err != nil {
			continue
		}
		ip, err := c.ExternalIP(ctx)
		if err != nil {
			continue
		}
		return c, ip, nil
	}
}

// parseSSDPLocation extracts the description URL from an SSDP search reply.
// The URL must point at the device that answered, on a private address: a
// reply is unauthenticated, so nothing else on the network gets to steer this
// process into fetching an arbitrary URL.
func parseSSDPLocation(reply []byte, from net.IP) (string, bool) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(reply)), nil)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", false
	}
	if !sameLANHost(loc, from) {
		return "", false
	}
	return loc, true
}

// sameLANHost reports whether rawURL is an http URL whose host is exactly
// the private or link-local address from.
func sameLANHost(rawURL string, from net.IP) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := net.ParseIP(u.Hostname())
	if host == nil || !host.Equal(from) {
		return false
	}
	return host.IsPrivate() || host.IsLinkLocalUnicast()
}

func newUPnPClient(ctx context.Context, location string, routerIP net.IP) (*upnpClient, error) {
	hc := &http.Client{
		Timeout: 2 * time.Second,
		// A router has no reason to redirect its own description; refusing
		// keeps every request pinned to the device that answered.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("description fetch: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDescriptionBytes))
	if err != nil {
		return nil, err
	}
	controlURL, serviceType, err := findWANService(body, location)
	if err != nil {
		return nil, err
	}
	if !sameLANHost(controlURL, routerIP) {
		return nil, errors.New("control URL points away from the router")
	}
	return &upnpClient{http: hc, routerIP: routerIP, controlURL: controlURL, serviceType: serviceType}, nil
}

// upnpDevice mirrors the parts of a UPnP device description we need. Devices
// nest (InternetGatewayDevice > WANDevice > WANConnectionDevice), so the
// search walks the tree.
type upnpDevice struct {
	Services []struct {
		ServiceType string `xml:"serviceType"`
		ControlURL  string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

type upnpRoot struct {
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

// findWANService parses a device description and returns the absolute control
// URL and type of the best WAN connection service in it.
func findWANService(desc []byte, location string) (controlURL, serviceType string, err error) {
	var root upnpRoot
	if err := xml.Unmarshal(desc, &root); err != nil {
		return "", "", fmt.Errorf("parse device description: %w", err)
	}
	found := map[string]string{}
	var walk func(d upnpDevice)
	walk = func(d upnpDevice) {
		for _, s := range d.Services {
			st := strings.TrimSpace(s.ServiceType)
			if _, dup := found[st]; !dup {
				found[st] = strings.TrimSpace(s.ControlURL)
			}
		}
		for _, child := range d.Devices {
			walk(child)
		}
	}
	walk(root.Device)

	base := location
	if b := strings.TrimSpace(root.URLBase); b != "" {
		base = b
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", "", err
	}
	for _, st := range wanServiceTypes {
		ctl, ok := found[st]
		if !ok || ctl == "" {
			continue
		}
		ref, err := url.Parse(ctl)
		if err != nil {
			continue
		}
		return baseURL.ResolveReference(ref).String(), st, nil
	}
	return "", "", errors.New("router description has no WAN connection service")
}

func (c *upnpClient) Protocol() string { return ProtoUPnP }

func (c *upnpClient) ExternalIP(ctx context.Context) (net.IP, error) {
	body, err := c.soap(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(strings.TrimSpace(soapValue(body, "NewExternalIPAddress")))
	if ip == nil || ip.IsUnspecified() {
		return nil, ErrExternalIPUnknown
	}
	return ip, nil
}

func (c *upnpClient) AddTCP(ctx context.Context, extPort, intPort int, lifetime time.Duration) (Mapping, error) {
	local, err := localIPFor(c.routerIP)
	if err != nil {
		return Mapping{}, err
	}
	lease := int(lifetime / time.Second)
	err = c.addMapping(ctx, extPort, intPort, local, lease)
	var se *SOAPError
	if errors.As(err, &se) && se.Code == upnpErrOnlyPermanentLeases {
		lease = 0
		err = c.addMapping(ctx, extPort, intPort, local, lease)
	}
	if errors.As(err, &se) && se.Code == upnpErrConflict && c.ownsMapping(ctx, extPort) {
		// Our own earlier forward, pointing at an address this machine no longer
		// has (a new DHCP lease). Replace it; never touch another device's.
		_ = c.DeleteTCP(ctx, extPort, intPort)
		err = c.addMapping(ctx, extPort, intPort, local, lease)
	}
	if err != nil {
		return Mapping{}, upnpError(err)
	}
	m := Mapping{
		Protocol:     ProtoUPnP,
		InternalIP:   local.String(),
		ExternalPort: extPort,
		InternalPort: intPort,
		Lifetime:     time.Duration(lease) * time.Second,
	}
	if ip, err := c.ExternalIP(ctx); err == nil {
		m.ExternalIP = ip.String()
	}
	return m, nil
}

func (c *upnpClient) addMapping(ctx context.Context, extPort, intPort int, local net.IP, lease int) error {
	_, err := c.soap(ctx, "AddPortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(extPort)},
		{"NewProtocol", "TCP"},
		{"NewInternalPort", strconv.Itoa(intPort)},
		{"NewInternalClient", local.String()},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", Description},
		{"NewLeaseDuration", strconv.Itoa(lease)},
	})
	return err
}

// ownsMapping reports whether the existing forward on extPort was created by
// this package, judged by its description.
func (c *upnpClient) ownsMapping(ctx context.Context, extPort int) bool {
	body, err := c.soap(ctx, "GetSpecificPortMappingEntry", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(extPort)},
		{"NewProtocol", "TCP"},
	})
	return err == nil && strings.TrimSpace(soapValue(body, "NewPortMappingDescription")) == Description
}

func (c *upnpClient) DeleteTCP(ctx context.Context, extPort, _ int) error {
	_, err := c.soap(ctx, "DeletePortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(extPort)},
		{"NewProtocol", "TCP"},
	})
	return err
}

// soap calls action on the WAN service. Arguments keep their order: some
// routers reject a request whose arguments are not in the order of the spec.
func (c *upnpClient) soap(ctx context.Context, action string, args [][2]string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, c.serviceType)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>%s</%s>", a[0], html.EscapeString(a[1]), a[0])
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.controlURL, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, c.serviceType, action))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDescriptionBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if se := parseSOAPFault(body); se != nil {
			return nil, se
		}
		return nil, fmt.Errorf("UPnP %s: %s", action, resp.Status)
	}
	return body, nil
}

// soapValue returns the text of the first element named name, ignoring
// namespaces. SOAP replies are small and flat, so a token scan is enough.
func soapValue(body []byte, name string) string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == name {
			var v string
			if err := dec.DecodeElement(&v, &se); err != nil {
				return ""
			}
			return v
		}
	}
}

// parseSOAPFault extracts the UPnP error from a fault body, or nil when the
// body carries none.
func parseSOAPFault(body []byte) *SOAPError {
	code, err := strconv.Atoi(strings.TrimSpace(soapValue(body, "errorCode")))
	if err != nil {
		return nil
	}
	return &SOAPError{Code: code, Description: strings.TrimSpace(soapValue(body, "errorDescription"))}
}

// upnpError maps the faults a user can act on to this package's shared errors.
func upnpError(err error) error {
	var se *SOAPError
	if !errors.As(err, &se) {
		return err
	}
	switch se.Code {
	case upnpErrConflict:
		return ErrConflict
	case upnpErrNotAuthorized:
		return ErrNotAuthorized
	}
	return err
}
