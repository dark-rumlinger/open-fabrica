// Package portmap talks to the router this machine sits behind, over the three
// LAN protocols consumer routers speak for this purpose: NAT-PMP, PCP, and UPnP
// IGD. It reads the router's external (internet) address and asks it to forward
// ports to this machine. Every packet stays on the local network: nothing here
// contacts a service on the internet.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"openfabrica/internal/msgcat"
)

// Protocol names, as reported in Router.Protocol and Mapping.Protocol.
const (
	ProtoNATPMP = "natpmp"
	ProtoPCP    = "pcp"
	ProtoUPnP   = "upnp"
)

// Description labels the mappings this package creates, so the router's own
// port-forwarding table shows who asked for them.
const Description = "Open Fabrica"

// discoverTimeout bounds the whole discovery pass. The three protocols are
// probed in parallel, so this is the wait for a network with no router at all.
const discoverTimeout = 2500 * time.Millisecond

var (
	// ErrNoRouter means no router answered on any of the three protocols:
	// either there is no router, or port mapping is switched off in it.
	ErrNoRouter = errors.New(msgcat.Mark("no router answered: automatic port forwarding (UPnP or NAT-PMP) is off in the router, or there is no router"))

	// ErrConflict means the router already forwards the port to another device.
	ErrConflict = errors.New(msgcat.Mark("the router already forwards this port to another device"))

	// ErrNotAuthorized means the router speaks the protocol but refuses to
	// create mappings (a common router setting).
	ErrNotAuthorized = errors.New(msgcat.Mark("the router refused to open the port (its automatic port forwarding may be set to read-only)"))

	// ErrExternalIPUnknown means the protocol in use cannot report the external
	// address until a mapping exists (PCP).
	ErrExternalIPUnknown = errors.New(msgcat.Mark("the router did not report its external address"))
)

// Router describes a router that answered discovery.
type Router struct {
	Protocol   string
	ExternalIP string // empty when the protocol cannot report it without a mapping
}

// Mapping is one forwarded port as granted by the router. Lifetime is the lease
// the router granted, which can be shorter than requested; zero means the
// mapping does not expire.
type Mapping struct {
	Protocol     string
	InternalIP   string
	ExternalIP   string
	ExternalPort int
	InternalPort int
	Lifetime     time.Duration
}

// Client is a router that can be asked to forward TCP ports.
type Client interface {
	Protocol() string
	ExternalIP(ctx context.Context) (net.IP, error)
	// AddTCP forwards extPort on the router to intPort on this machine. The
	// router must grant exactly extPort: a different port is useless for a
	// website, so implementations return ErrConflict instead.
	AddTCP(ctx context.Context, extPort, intPort int, lifetime time.Duration) (Mapping, error)
	DeleteTCP(ctx context.Context, extPort, intPort int) error
}

// Discover finds the router and returns a client for it. All three protocols
// are probed at once and the first answer that includes the external address
// wins. PCP cannot report that address before a mapping exists, so a PCP
// answer is only used when neither NAT-PMP nor UPnP answers.
func Discover(ctx context.Context) (Client, Router, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	type result struct {
		client Client
		ip     net.IP
		err    error
	}
	results := make(chan result, 3)
	probe := func(fn func(context.Context) (Client, net.IP, error)) {
		go func() {
			c, ip, err := fn(ctx)
			results <- result{c, ip, err}
		}()
	}

	pending := 1
	probe(probeUPnP)
	if gw, err := defaultGateway(); err == nil {
		pending += 2
		probe(func(ctx context.Context) (Client, net.IP, error) { return probeNATPMP(ctx, gw) })
		probe(func(ctx context.Context) (Client, net.IP, error) { return probePCP(ctx, gw) })
	}

	var fallback Client
	for ; pending > 0; pending-- {
		r := <-results
		if r.err != nil || r.client == nil {
			continue
		}
		if r.ip == nil {
			fallback = r.client
			continue
		}
		return r.client, Router{Protocol: r.client.Protocol(), ExternalIP: r.ip.String()}, nil
	}
	if fallback != nil {
		return fallback, Router{Protocol: fallback.Protocol()}, nil
	}
	return nil, Router{}, ErrNoRouter
}

// localIPFor returns this machine's address on the interface that routes to
// remote. Dialing UDP only consults the routing table; no packet is sent.
func localIPFor(remote net.IP) (net.IP, error) {
	conn, err := net.Dial("udp4", net.JoinHostPort(remote.String(), "9"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return nil, fmt.Errorf("no local address routes to %s", remote)
	}
	return addr.IP.To4(), nil
}

// IsCarrierNAT reports whether ip is in the carrier-grade NAT range
// (100.64.0.0/10, RFC 6598). A router whose "external" address is in it shares
// one internet address with other customers of the provider, so nothing can
// be forwarded to it from the internet.
func IsCarrierNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64
}

// udpRoundTrip sends req to addr and waits for a reply that accept approves,
// retransmitting with a doubling timeout (250ms, 500ms, 1s) as RFC 6886 and
// RFC 6887 prescribe, cut short by ctx.
func udpRoundTrip(ctx context.Context, addr *net.UDPAddr, req []byte, accept func([]byte) bool) ([]byte, error) {
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 1100)
	wait := 250 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetReadDeadline(deadline)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break // timeout or ICMP refusal: retransmit or give up
			}
			if accept(buf[:n]) {
				return append([]byte(nil), buf[:n]...), nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		wait *= 2
	}
	return nil, errors.New("no reply")
}
