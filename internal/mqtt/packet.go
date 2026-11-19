package mqtt

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MQTT 3.1.1 control packet types (high nibble of byte 0).
const (
	pktConnect     byte = 1
	pktConnAck     byte = 2
	pktPublish     byte = 3
	pktPubAck      byte = 4
	pktSubscribe   byte = 8
	pktSubAck      byte = 9
	pktUnsubscribe byte = 10
	pktUnsubAck    byte = 11
	pktPingReq     byte = 12
	pktPingResp    byte = 13
	pktDisconnect  byte = 14
)

// CONNACK return codes (MQTT 3.1.1 section 3.2.2.3).
const (
	connAccepted          byte = 0x00
	connUnacceptableProto byte = 0x01
	connBadUserOrPass     byte = 0x04
	connNotAuthorized     byte = 0x05
)

// MaxPacketSize bounds incoming control packet remaining length (1 MiB) so a
// malformed or hostile client cannot trigger unbounded allocations.
const MaxPacketSize = 1 << 20

var (
	ErrPacketTooLarge = errors.New("mqtt packet exceeds maximum size")
	ErrMalformed      = errors.New("malformed mqtt packet")
)

type packet struct {
	ptype byte
	flags byte
	body  []byte
}

type connectPacket struct {
	clientID   string
	username   string
	password   string
	cleanSess  bool
	keepAlive  uint16
	willFlag   bool
	willQoS    byte
	willRetain bool
	willTopic  string
	willMsg    []byte
}

type publishPacket struct {
	dup      bool
	qos      byte
	retain   bool
	topic    string
	packetID uint16
	payload  []byte
}

type subItem struct {
	filter string
	qos    byte
}

func readPacket(r *bufio.Reader) (packet, error) {
	b0, err := r.ReadByte()
	if err != nil {
		return packet{}, err
	}
	remLen, err := readVarInt(r)
	if err != nil {
		return packet{}, err
	}
	if remLen > MaxPacketSize {
		return packet{}, ErrPacketTooLarge
	}
	body := make([]byte, remLen)
	if remLen > 0 {
		if _, err := io.ReadFull(r, body); err != nil {
			return packet{}, err
		}
	}
	return packet{
		ptype: b0 >> 4,
		flags: b0 & 0x0f,
		body:  body,
	}, nil
}

func writePacket(w io.Writer, ptype, flags byte, body []byte) error {
	var hdr [5]byte
	hdr[0] = (ptype << 4) | (flags & 0x0f)
	n := encodeVarInt(hdr[1:], len(body))
	var buf bytes.Buffer
	buf.Grow(1 + n + len(body))
	buf.Write(hdr[:1+n])
	buf.Write(body)
	_, err := w.Write(buf.Bytes())
	return err
}

func readVarInt(r *bufio.Reader) (int, error) {
	var multiplier = 1
	var value int
	for range 4 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value += int(b&0x7f) * multiplier
		if (b & 0x80) == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, ErrMalformed
}

func encodeVarInt(dst []byte, x int) int {
	i := 0
	for {
		b := byte(x % 128)
		x /= 128
		if x > 0 {
			b |= 0x80
		}
		dst[i] = b
		i++
		if x == 0 {
			return i
		}
	}
}

func readUTF8(b []byte) (string, []byte, error) {
	if len(b) < 2 {
		return "", nil, ErrMalformed
	}
	n := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if len(b) < n {
		return "", nil, ErrMalformed
	}
	return string(b[:n]), b[n:], nil
}

func readBytes(b []byte) ([]byte, []byte, error) {
	if len(b) < 2 {
		return nil, nil, ErrMalformed
	}
	n := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if len(b) < n {
		return nil, nil, ErrMalformed
	}
	cp := make([]byte, n)
	copy(cp, b[:n])
	return cp, b[n:], nil
}

func writeUTF8(buf *bytes.Buffer, s string) {
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(s)))
	buf.Write(lenBuf[:])
	buf.WriteString(s)
}

func parseConnect(body []byte) (connectPacket, byte, error) {
	proto, rest, err := readUTF8(body)
	if err != nil {
		return connectPacket{}, connUnacceptableProto, err
	}
	if len(rest) < 4 {
		return connectPacket{}, connUnacceptableProto, ErrMalformed
	}
	level := rest[0]
	if (proto != "MQTT" || level != 4) && (proto != "MQIsdp" || level != 3) {
		return connectPacket{}, connUnacceptableProto, fmt.Errorf("unsupported protocol %q level %d", proto, level)
	}
	flags := rest[1]
	keepAlive := binary.BigEndian.Uint16(rest[2:4])
	rest = rest[4:]

	clientID, rest, err := readUTF8(rest)
	if err != nil {
		return connectPacket{}, connUnacceptableProto, err
	}

	cp := connectPacket{
		clientID:   clientID,
		cleanSess:  flags&0x02 != 0,
		keepAlive:  keepAlive,
		willFlag:   flags&0x04 != 0,
		willQoS:    (flags >> 3) & 0x03,
		willRetain: flags&0x20 != 0,
	}
	if cp.willFlag {
		cp.willTopic, rest, err = readUTF8(rest)
		if err != nil {
			return connectPacket{}, connUnacceptableProto, err
		}
		cp.willMsg, rest, err = readBytes(rest)
		if err != nil {
			return connectPacket{}, connUnacceptableProto, err
		}
	}
	if flags&0x80 != 0 {
		cp.username, rest, err = readUTF8(rest)
		if err != nil {
			return connectPacket{}, connBadUserOrPass, err
		}
	}
	if flags&0x40 != 0 {
		var pw []byte
		pw, _, err = readBytes(rest)
		if err != nil {
			return connectPacket{}, connBadUserOrPass, err
		}
		cp.password = string(pw)
	}
	return cp, connAccepted, nil
}

func parsePublish(flags byte, body []byte) (publishPacket, error) {
	topic, rest, err := readUTF8(body)
	if err != nil {
		return publishPacket{}, err
	}
	qos := (flags >> 1) & 0x03
	if qos > 1 {
		// MQTT 3.1.1: QoS 2 is not used here; clamp/reject malformed.
		return publishPacket{}, ErrMalformed
	}
	var pktID uint16
	if qos > 0 {
		if len(rest) < 2 {
			return publishPacket{}, ErrMalformed
		}
		pktID = binary.BigEndian.Uint16(rest[:2])
		rest = rest[2:]
	}
	payload := make([]byte, len(rest))
	copy(payload, rest)
	return publishPacket{
		dup:      flags&0x08 != 0,
		qos:      qos,
		retain:   flags&0x01 != 0,
		topic:    topic,
		packetID: pktID,
		payload:  payload,
	}, nil
}

func encodePublish(topic string, payload []byte, qos byte, retain bool, pktID uint16) ([]byte, byte) {
	var flags byte
	if retain {
		flags |= 0x01
	}
	flags |= (qos & 0x03) << 1
	var buf bytes.Buffer
	buf.Grow(2 + len(topic) + 2 + len(payload))
	writeUTF8(&buf, topic)
	if qos > 0 {
		var idBuf [2]byte
		binary.BigEndian.PutUint16(idBuf[:], pktID)
		buf.Write(idBuf[:])
	}
	buf.Write(payload)
	return buf.Bytes(), flags
}

func parseSubscribe(body []byte) (uint16, []subItem, error) {
	if len(body) < 2 {
		return 0, nil, ErrMalformed
	}
	pktID := binary.BigEndian.Uint16(body[:2])
	rest := body[2:]
	var items []subItem
	for len(rest) > 0 {
		var f string
		var err error
		f, rest, err = readUTF8(rest)
		if err != nil || len(rest) < 1 {
			return 0, nil, ErrMalformed
		}
		qos := rest[0] & 0x03
		rest = rest[1:]
		items = append(items, subItem{filter: f, qos: qos})
	}
	if len(items) == 0 {
		return 0, nil, ErrMalformed
	}
	return pktID, items, nil
}

func parseUnsubscribe(body []byte) (uint16, []string, error) {
	if len(body) < 2 {
		return 0, nil, ErrMalformed
	}
	pktID := binary.BigEndian.Uint16(body[:2])
	rest := body[2:]
	var filters []string
	for len(rest) > 0 {
		var f string
		var err error
		f, rest, err = readUTF8(rest)
		if err != nil {
			return 0, nil, ErrMalformed
		}
		filters = append(filters, f)
	}
	if len(filters) == 0 {
		return 0, nil, ErrMalformed
	}
	return pktID, filters, nil
}
