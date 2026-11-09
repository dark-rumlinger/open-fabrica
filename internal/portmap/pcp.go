package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// PCP (RFC 6887), the successor to NAT-PMP on the same UDP port. It has no
// "what is my external address" request: the address arrives in the answer to
// a MAP request, so ExternalIP only knows it once a mapping exists.
const (
	pcpVersion    = 2
	pcpOpAnnounce = 0
	pcpOpMap      = 1
	pcpProtoTCP   = 6
	pcpHeaderLen  = 24
	pcpMapLen     = 36
)

type pcpClient struct {
	gateway *net.UDPAddr

	mu     sync.Mutex
	nonces map[int][12]byte // by internal port; a delete must repeat the MAP nonce
	extIP  net.IP
}

func probePCP(ctx context.Context, gw net.IP) (Client, net.IP, error) {
	c := &pcpClient{gateway: &net.UDPAddr{IP: gw, Port: pmpPort}, nonces: map[int][12]byte{}}
	local, err := localIPFor(gw)
	if err != nil {
		return nil, nil, err
	}
	req := encodePCPHeader(pcpOpAnnounce, 0, local)
	resp, err := udpRoundTrip(ctx, c.gateway, req, isPCPResponse(pcpOpAnnounce))
	if err != nil {
		return nil, nil, err
	}
	if err := pcpResult(resp[3]); err != nil {
		return nil, nil, err
	}
	return c, nil, nil
}

func (c *pcpClient) Protocol() string { return ProtoPCP }

func (c *pcpClient) ExternalIP(context.Context) (net.IP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.extIP == nil {
		return nil, ErrExternalIPUnknown
	}
	return c.extIP, nil
}

func (c *pcpClient) AddTCP(ctx context.Context, extPort, intPort int, lifetime time.Duration) (Mapping, error) {
	c.mu.Lock()
	nonce, ok := c.nonces[intPort]
	c.mu.Unlock()
	if !ok {
		if _, err := rand.Read(nonce[:]); err != nil {
			return Mapping{}, err
		}
	}
	local, err := localIPFor(c.gateway.IP)
	if err != nil {
		return Mapping{}, err
	}
	res, err := c.doMap(ctx, local, nonce, intPort, extPort, uint32(lifetime/time.Second))
	if err != nil {
		return Mapping{}, err
	}
	c.mu.Lock()
	c.nonces[intPort] = nonce
	if res.externalIP != nil {
		c.extIP = res.externalIP
	}
	c.mu.Unlock()
	if res.externalPort != extPort {
		_, _ = c.doMap(ctx, local, nonce, intPort, 0, 0)
		return Mapping{}, ErrConflict
	}
	m := Mapping{
		Protocol:     ProtoPCP,
		InternalIP:   local.String(),
		ExternalPort: res.externalPort,
		InternalPort: intPort,
		Lifetime:     time.Duration(res.lifetime) * time.Second,
	}
	if res.externalIP != nil {
		m.ExternalIP = res.externalIP.String()
	}
	return m, nil
}

func (c *pcpClient) DeleteTCP(ctx context.Context, _, intPort int) error {
	c.mu.Lock()
	nonce, ok := c.nonces[intPort]
	c.mu.Unlock()
	if !ok {
		return nil // never mapped by this client
	}
	local, err := localIPFor(c.gateway.IP)
	if err != nil {
		return err
	}
	_, err = c.doMap(ctx, local, nonce, intPort, 0, 0)
	return err
}

type pcpMapResult struct {
	lifetime     uint32
	externalPort int
	externalIP   net.IP
}

func (c *pcpClient) doMap(ctx context.Context, local net.IP, nonce [12]byte, intPort, extPort int, lifetime uint32) (pcpMapResult, error) {
	req := encodePCPMap(local, nonce, intPort, extPort, lifetime)
	resp, err := udpRoundTrip(ctx, c.gateway, req, func(b []byte) bool {
		return isPCPResponse(pcpOpMap)(b) && len(b) >= pcpHeaderLen+pcpMapLen &&
			[12]byte(b[pcpHeaderLen:pcpHeaderLen+12]) == nonce
	})
	if err != nil {
		return pcpMapResult{}, err
	}
	return parsePCPMap(resp)
}

func isPCPResponse(op byte) func([]byte) bool {
	return func(b []byte) bool {
		return len(b) >= pcpHeaderLen && b[0] == pcpVersion && b[1] == 0x80|op
	}
}

// encodePCPHeader builds the 24-byte request header. The client address is
// sent as an IPv4-mapped IPv6 address, as RFC 6887 requires.
func encodePCPHeader(op byte, lifetime uint32, client net.IP) []byte {
	b := make([]byte, pcpHeaderLen)
	b[0] = pcpVersion
	b[1] = op
	binary.BigEndian.PutUint32(b[4:], lifetime)
	copy(b[8:24], client.To16())
	return b
}

// encodePCPMap builds a 60-byte MAP request for a TCP port, with "any IPv4
// address" (::ffff:0.0.0.0) as the suggested external address.
func encodePCPMap(client net.IP, nonce [12]byte, intPort, extPort int, lifetime uint32) []byte {
	b := append(encodePCPHeader(pcpOpMap, lifetime, client), make([]byte, pcpMapLen)...)
	p := b[pcpHeaderLen:]
	copy(p[0:12], nonce[:])
	p[12] = pcpProtoTCP
	binary.BigEndian.PutUint16(p[16:], uint16(intPort))
	binary.BigEndian.PutUint16(p[18:], uint16(extPort))
	copy(p[20:36], net.IPv4zero.To16())
	return b
}

// parsePCPMap decodes a MAP response: result code and granted lifetime from
// the header, assigned external port and address from the MAP payload.
func parsePCPMap(b []byte) (pcpMapResult, error) {
	if len(b) < pcpHeaderLen+pcpMapLen {
		return pcpMapResult{}, errors.New("short PCP response")
	}
	if err := pcpResult(b[3]); err != nil {
		return pcpMapResult{}, err
	}
	p := b[pcpHeaderLen:]
	return pcpMapResult{
		lifetime:     binary.BigEndian.Uint32(b[4:]),
		externalPort: int(binary.BigEndian.Uint16(p[18:])),
		externalIP:   net.IP(append([]byte(nil), p[20:36]...)).To4(),
	}, nil
}

func pcpResult(code byte) error {
	switch code {
	case 0:
		return nil
	case 2:
		return ErrNotAuthorized
	default:
		return fmt.Errorf("PCP result code %d", code)
	}
}
