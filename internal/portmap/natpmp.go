package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// NAT-PMP (RFC 6886): fixed-size UDP messages to the default gateway on port
// 5351. Opcode 0 asks for the external address, opcode 2 maps a TCP port.
const (
	pmpPort          = 5351
	natpmpOpExternal = 0
	natpmpOpMapTCP   = 2
)

type natpmpClient struct {
	gateway *net.UDPAddr
}

func probeNATPMP(ctx context.Context, gw net.IP) (Client, net.IP, error) {
	c := &natpmpClient{gateway: &net.UDPAddr{IP: gw, Port: pmpPort}}
	ip, err := c.ExternalIP(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, ip, nil
}

func (c *natpmpClient) Protocol() string { return ProtoNATPMP }

func (c *natpmpClient) ExternalIP(ctx context.Context) (net.IP, error) {
	resp, err := udpRoundTrip(ctx, c.gateway, []byte{0, natpmpOpExternal}, func(b []byte) bool {
		return len(b) >= 2 && b[0] == 0 && b[1] == 128+natpmpOpExternal
	})
	if err != nil {
		return nil, err
	}
	return parseNATPMPExternal(resp)
}

func (c *natpmpClient) AddTCP(ctx context.Context, extPort, intPort int, lifetime time.Duration) (Mapping, error) {
	granted, gotExt, err := c.mapTCP(ctx, extPort, intPort, uint32(lifetime/time.Second))
	if err != nil {
		return Mapping{}, err
	}
	if gotExt != extPort {
		// The router picked another external port, which no browser will ask
		// for. Hand it back rather than leave a useless forward behind.
		_, _, _ = c.mapTCP(ctx, 0, intPort, 0)
		return Mapping{}, ErrConflict
	}
	local, _ := localIPFor(c.gateway.IP)
	m := Mapping{
		Protocol:     ProtoNATPMP,
		ExternalPort: gotExt,
		InternalPort: intPort,
		Lifetime:     time.Duration(granted) * time.Second,
	}
	if local != nil {
		m.InternalIP = local.String()
	}
	if ip, err := c.ExternalIP(ctx); err == nil {
		m.ExternalIP = ip.String()
	}
	return m, nil
}

// DeleteTCP removes the mapping for intPort: RFC 6886 deletes by internal port
// with a zero lifetime and zero suggested external port.
func (c *natpmpClient) DeleteTCP(ctx context.Context, _, intPort int) error {
	_, _, err := c.mapTCP(ctx, 0, intPort, 0)
	return err
}

func (c *natpmpClient) mapTCP(ctx context.Context, extPort, intPort int, lifetime uint32) (granted uint32, gotExt int, err error) {
	req := encodeNATPMPMap(intPort, extPort, lifetime)
	resp, err := udpRoundTrip(ctx, c.gateway, req, func(b []byte) bool {
		return len(b) >= 2 && b[0] == 0 && b[1] == 128+natpmpOpMapTCP
	})
	if err != nil {
		return 0, 0, err
	}
	return parseNATPMPMap(resp, intPort)
}

// encodeNATPMPMap builds the 12-byte TCP mapping request.
func encodeNATPMPMap(intPort, extPort int, lifetime uint32) []byte {
	b := make([]byte, 12)
	b[0] = 0
	b[1] = natpmpOpMapTCP
	binary.BigEndian.PutUint16(b[4:], uint16(intPort))
	binary.BigEndian.PutUint16(b[6:], uint16(extPort))
	binary.BigEndian.PutUint32(b[8:], lifetime)
	return b
}

// parseNATPMPExternal decodes the 12-byte external address response.
func parseNATPMPExternal(b []byte) (net.IP, error) {
	if len(b) < 12 {
		return nil, errors.New("short NAT-PMP response")
	}
	if err := natpmpResult(binary.BigEndian.Uint16(b[2:])); err != nil {
		return nil, err
	}
	return net.IPv4(b[8], b[9], b[10], b[11]).To4(), nil
}

// parseNATPMPMap decodes the 16-byte mapping response and checks it answers
// the request for intPort.
func parseNATPMPMap(b []byte, intPort int) (lifetime uint32, extPort int, err error) {
	if len(b) < 16 {
		return 0, 0, errors.New("short NAT-PMP response")
	}
	if err := natpmpResult(binary.BigEndian.Uint16(b[2:])); err != nil {
		return 0, 0, err
	}
	if got := int(binary.BigEndian.Uint16(b[8:])); got != intPort {
		return 0, 0, fmt.Errorf("NAT-PMP answered for port %d, asked for %d", got, intPort)
	}
	return binary.BigEndian.Uint32(b[12:]), int(binary.BigEndian.Uint16(b[10:])), nil
}

func natpmpResult(code uint16) error {
	switch code {
	case 0:
		return nil
	case 2:
		return ErrNotAuthorized
	default:
		return fmt.Errorf("NAT-PMP result code %d", code)
	}
}
