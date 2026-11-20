package mqtt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bridgeDialTimeout    = 10 * time.Second
	bridgeKeepAliveSec   = 60
	bridgePingInterval   = 20 * time.Second
	bridgeOutQueueSize   = 128
	bridgeInitialBackoff = 1 * time.Second
	bridgeMaxBackoff     = 30 * time.Second
)

// BridgeConfig defines an outbound MQTT 3.1.1 bridge to an external broker
// (e.g. LAN Mosquitto, Zigbee2MQTT, EMQX, Home Assistant, Frigate, Meshtastic).
type BridgeConfig struct {
	Name         string
	BrokerURL    string // tcp://, mqtt://, tls://, ssl://, mqtts://
	ClientID     string // unique client ID sent in CONNECT
	Username     string
	Password     string
	RemoteSub    []string // remote topic filters subscribed to and injected locally
	LocalPrefix  string   // optional prefix prepended to incoming remote topics (e.g. "lan/")
	LocalPub     []string // local topic filters forwarded out to the remote broker
	RemotePrefix string   // optional prefix prepended to outgoing remote topics (after stripping LocalPrefix)
	QoS          byte     // 0 or 1
}

// BridgeStats reports live status and counters for one outbound bridge.
type BridgeStats struct {
	Name            string     `json:"name"`
	BrokerURL       string     `json:"broker_url"`
	Connected       bool       `json:"connected"`
	LastError       string     `json:"last_error,omitempty"`
	LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
	MessagesIn      uint64     `json:"messages_in"`
	MessagesOut     uint64     `json:"messages_out"`
}

// ValidateBridgeURL parses and validates an MQTT bridge broker URL.
// Supported schemes: tcp://, mqtt:// (default :1883), tls://, ssl://, mqtts:// (default :8883).
// Private/LAN addresses are permitted for local-first IoT stacks; cloud metadata endpoints are blocked.
func ValidateBridgeURL(raw string) (addr, serverName string, useTLS bool, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false, fmt.Errorf("invalid broker_url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	var defaultPort string
	switch scheme {
	case "tcp", "mqtt":
		defaultPort = "1883"
	case "tls", "ssl", "mqtts":
		defaultPort = "8883"
		useTLS = true
	default:
		return "", "", false, fmt.Errorf("unsupported broker_url scheme %q (use tcp://, mqtt://, tls://, ssl://, or mqtts://)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", false, errors.New("broker_url must include a host")
	}
	if isCloudMetadataHost(host) {
		return "", "", false, fmt.Errorf("broker_url host %q is blocked", host)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(host, port), host, useTLS, nil
}

func isCloudMetadataHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	switch h {
	case "169.254.169.254", "169.254.170.2", "metadata.google.internal", "metadata.goog", "fd00:ec2::254":
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLinkLocalUnicast() {
		return true
	}
	return false
}

func bridgeConfigEqual(a, b BridgeConfig) bool {
	return a.Name == b.Name &&
		a.BrokerURL == b.BrokerURL &&
		a.ClientID == b.ClientID &&
		a.Username == b.Username &&
		a.Password == b.Password &&
		a.LocalPrefix == b.LocalPrefix &&
		a.RemotePrefix == b.RemotePrefix &&
		a.QoS == b.QoS &&
		slices.Equal(a.RemoteSub, b.RemoteSub) &&
		slices.Equal(a.LocalPub, b.LocalPub)
}

func remapInboundTopic(localPrefix, remoteTopic string) string {
	if localPrefix == "" {
		return remoteTopic
	}
	if strings.HasSuffix(localPrefix, "/") {
		return localPrefix + strings.TrimPrefix(remoteTopic, "/")
	}
	return localPrefix + "/" + strings.TrimPrefix(remoteTopic, "/")
}

func remapOutboundTopic(localPrefix, remotePrefix, localTopic string) string {
	t := localTopic
	if localPrefix != "" {
		lp := localPrefix
		if !strings.HasSuffix(lp, "/") {
			lp += "/"
		}
		if strings.HasPrefix(t, lp) {
			t = strings.TrimPrefix(t, lp)
		}
	}
	if remotePrefix == "" {
		return t
	}
	if strings.HasSuffix(remotePrefix, "/") {
		return remotePrefix + strings.TrimPrefix(t, "/")
	}
	return remotePrefix + "/" + strings.TrimPrefix(t, "/")
}

func encodeConnect(clientID, username, password string, keepAlive uint16) []byte {
	var b bytes.Buffer
	writeUTF8(&b, "MQTT")
	b.WriteByte(4)        // MQTT 3.1.1
	var flags byte = 0x02 // cleanSession = true
	if username != "" {
		flags |= 0x80
	}
	if password != "" {
		flags |= 0x40
	}
	b.WriteByte(flags)
	var ka [2]byte
	binary.BigEndian.PutUint16(ka[:], keepAlive)
	b.Write(ka[:])
	writeUTF8(&b, clientID)
	if username != "" {
		writeUTF8(&b, username)
	}
	if password != "" {
		var pl [2]byte
		binary.BigEndian.PutUint16(pl[:], uint16(len(password)))
		b.Write(pl[:])
		b.WriteString(password)
	}
	return b.Bytes()
}

func encodeSubscribe(pktID uint16, filters []string, qos byte) []byte {
	var b bytes.Buffer
	var id [2]byte
	binary.BigEndian.PutUint16(id[:], pktID)
	b.Write(id[:])
	for _, f := range filters {
		writeUTF8(&b, f)
		b.WriteByte(min(qos, 1))
	}
	return b.Bytes()
}

type bridgeOutPub struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

type bridgeRunner struct {
	cfg    BridgeConfig
	broker *Broker
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	outCh  chan bridgeOutPub

	nextPktID atomic.Uint32
	msgsIn    atomic.Uint64
	msgsOut   atomic.Uint64

	mu              sync.RWMutex
	connected       bool
	lastError       string
	lastConnectedAt *time.Time
}

func newBridgeRunner(cfg BridgeConfig, b *Broker) *bridgeRunner {
	if cfg.QoS > 1 {
		cfg.QoS = 1
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "of-bridge-" + cfg.Name
	}
	ctx, cancel := context.WithCancel(context.Background())
	br := &bridgeRunner{
		cfg:    cfg,
		broker: b,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		outCh:  make(chan bridgeOutPub, bridgeOutQueueSize),
	}
	go br.run()
	return br
}

func (br *bridgeRunner) allocPacketID() uint16 {
	for {
		if id := uint16(br.nextPktID.Add(1)); id != 0 {
			return id
		}
	}
}

func (br *bridgeRunner) stop() {
	br.cancel()
	<-br.done
}

func (br *bridgeRunner) enqueue(localTopic string, payload []byte, qos byte, retain bool) {
	if len(br.cfg.LocalPub) == 0 {
		return
	}
	matched := false
	for _, f := range br.cfg.LocalPub {
		if MatchTopic(f, localTopic) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	remoteTopic := remapOutboundTopic(br.cfg.LocalPrefix, br.cfg.RemotePrefix, localTopic)
	if ValidatePublishTopic(remoteTopic) != nil {
		return
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	outQoS := min(qos, br.cfg.QoS)
	select {
	case br.outCh <- bridgeOutPub{topic: remoteTopic, payload: cp, qos: outQoS, retain: retain}:
	default:
		// Drop when outbound queue is saturated so local broker never blocks.
	}
}

func (br *bridgeRunner) stats() BridgeStats {
	br.mu.RLock()
	st := BridgeStats{
		Name:        br.cfg.Name,
		BrokerURL:   br.cfg.BrokerURL,
		Connected:   br.connected,
		LastError:   br.lastError,
		MessagesIn:  br.msgsIn.Load(),
		MessagesOut: br.msgsOut.Load(),
	}
	if br.lastConnectedAt != nil {
		t := *br.lastConnectedAt
		st.LastConnectedAt = &t
	}
	br.mu.RUnlock()
	return st
}

func (br *bridgeRunner) setError(err error) {
	br.mu.Lock()
	br.connected = false
	if err != nil {
		br.lastError = err.Error()
	}
	br.mu.Unlock()
}

func (br *bridgeRunner) setConnected() {
	now := time.Now().UTC()
	br.mu.Lock()
	br.connected = true
	br.lastError = ""
	br.lastConnectedAt = &now
	br.mu.Unlock()
}

func (br *bridgeRunner) run() {
	defer close(br.done)
	backoff := bridgeInitialBackoff
	for {
		if br.ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := br.runSession()
		if br.ctx.Err() != nil {
			return
		}
		if err != nil {
			br.setError(err)
		} else {
			br.mu.Lock()
			br.connected = false
			br.mu.Unlock()
		}
		// Reset backoff if the session stayed connected for at least 15s.
		if time.Since(start) >= 15*time.Second {
			backoff = bridgeInitialBackoff
		}
		timer := time.NewTimer(backoff)
		select {
		case <-br.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > bridgeMaxBackoff {
			backoff = bridgeMaxBackoff
		}
	}
}

func (br *bridgeRunner) runSession() error {
	addr, serverName, useTLS, err := ValidateBridgeURL(br.cfg.BrokerURL)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: bridgeDialTimeout}
	var conn net.Conn
	if useTLS {
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config: &tls.Config{
				ServerName: serverName,
				MinVersion: tls.VersionTLS12,
			},
		}
		conn, err = tlsDialer.DialContext(br.ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(br.ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	// Close connection immediately when bridge context is cancelled.
	sessDone := make(chan struct{})
	defer close(sessDone)
	go func() {
		select {
		case <-br.ctx.Done():
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = writePacket(conn, pktDisconnect, 0, nil)
			_ = conn.Close()
		case <-sessDone:
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(connectHandshakeTimout))
	connectBody := encodeConnect(br.cfg.ClientID, br.cfg.Username, br.cfg.Password, bridgeKeepAliveSec)
	if err := writePacket(conn, pktConnect, 0, connectBody); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	r := bufio.NewReader(conn)
	ack, err := readPacket(r)
	if err != nil {
		return fmt.Errorf("read connack: %w", err)
	}
	if ack.ptype != pktConnAck || len(ack.body) < 2 {
		return errors.New("unexpected packet instead of CONNACK")
	}
	if ack.body[1] != connAccepted {
		return fmt.Errorf("remote broker rejected CONNECT (code %d)", ack.body[1])
	}

	if len(br.cfg.RemoteSub) > 0 {
		subBody := encodeSubscribe(br.allocPacketID(), br.cfg.RemoteSub, br.cfg.QoS)
		if err := writePacket(conn, pktSubscribe, 0x02, subBody); err != nil {
			return fmt.Errorf("send subscribe: %w", err)
		}
		subAck, err := readPacket(r)
		if err != nil {
			return fmt.Errorf("read suback: %w", err)
		}
		if subAck.ptype != pktSubAck {
			return errors.New("unexpected packet instead of SUBACK")
		}
	}

	_ = conn.SetDeadline(time.Time{})
	br.setConnected()

	var writeMu sync.Mutex
	writePkt := func(ptype, flags byte, body []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return writePacket(conn, ptype, flags, body)
	}

	// Writer + keepalive loop.
	writerErr := make(chan error, 1)
	writerStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(bridgePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-br.ctx.Done():
				return
			case <-writerStop:
				return
			case <-ticker.C:
				if err := writePkt(pktPingReq, 0, nil); err != nil {
					writerErr <- err
					_ = conn.Close()
					return
				}
			case msg := <-br.outCh:
				var pktID uint16
				if msg.qos > 0 {
					pktID = br.allocPacketID()
				}
				body, flags := encodePublish(msg.topic, msg.payload, msg.qos, msg.retain, pktID)
				if err := writePkt(pktPublish, flags, body); err != nil {
					writerErr <- err
					_ = conn.Close()
					return
				}
				br.msgsOut.Add(1)
			}
		}
	}()
	defer close(writerStop)

	readTimeout := time.Duration(bridgeKeepAliveSec) * time.Second * 3 / 2
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		p, err := readPacket(r)
		if err != nil {
			select {
			case werr := <-writerErr:
				return werr
			default:
				return err
			}
		}
		switch p.ptype {
		case pktPublish:
			pub, err := parsePublish(p.flags, p.body)
			if err != nil {
				return err
			}
			if pub.qos == 1 {
				var ack [2]byte
				binary.BigEndian.PutUint16(ack[:], pub.packetID)
				if err := writePkt(pktPubAck, 0, ack[:]); err != nil {
					return err
				}
			}
			localTopic := remapInboundTopic(br.cfg.LocalPrefix, pub.topic)
			if ValidatePublishTopic(localTopic) == nil {
				br.msgsIn.Add(1)
				_ = br.broker.PublishFromBridge(br.cfg.Name, localTopic, pub.payload, min(pub.qos, 1), pub.retain)
			}
		case pktPubAck, pktPingResp, pktSubAck, pktUnsubAck:
			// Expected control responses from remote broker.
		case pktDisconnect:
			return errors.New("remote broker disconnected")
		}
	}
}
