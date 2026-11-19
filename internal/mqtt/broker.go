package mqtt

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxClients      = 100
	defaultKeepAlive       = 60 * time.Second
	connectHandshakeTimout = 10 * time.Second
	clientSendQueueSize    = 64
)

// RetainedMessage represents a stored retained MQTT message.
type RetainedMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
}

// RetainedStore persists retained messages per project.
type RetainedStore interface {
	SetRetained(topic string, payload []byte, qos byte) error
	DeleteRetained(topic string) error
	ListRetained() ([]RetainedMessage, error)
}

// AuthResult describes an authenticated MQTT client's identity and topic ACLs.
type AuthResult struct {
	DeviceName string
	Username   string
	AllowPub   string // comma-separated MQTT topic filters
	AllowSub   string // comma-separated MQTT topic filters
}

// Authenticator validates a client's CONNECT credentials.
type Authenticator func(clientID, username, password string) (AuthResult, bool)

// PublishEvent is emitted whenever a valid PUBLISH arrives (from a client or LWT).
type PublishEvent struct {
	ClientID   string
	DeviceName string
	Username   string
	Topic      string
	Payload    []byte
	QoS        byte
	Retain     bool
}

// PublishHandler receives published messages for ingest rules / event bus forwarding.
type PublishHandler func(ev PublishEvent)

// BrokerConfig configures a per-project MQTT broker.
type BrokerConfig struct {
	MaxClients int
	Retained   RetainedStore
	Auth       Authenticator
	OnPublish  PublishHandler
}

// BrokerStats summarizes live broker state.
type BrokerStats struct {
	ConnectedClients int      `json:"connected_clients"`
	ClientIDs        []string `json:"client_ids"`
	Subscriptions    int      `json:"subscriptions"`
}

type outMsg struct {
	ptype byte
	flags byte
	body  []byte
}

type clientSession struct {
	id         string
	deviceName string
	username   string
	allowPub   string
	allowSub   string
	rw         io.ReadWriteCloser
	sendCh     chan outMsg
	subs       map[string]byte
	nextPktID  atomic.Uint32

	willFlag   bool
	willTopic  string
	willMsg    []byte
	willQoS    byte
	willRetain bool

	closeOnce sync.Once
	closed    chan struct{}
	cleanExit atomic.Bool
	replaced  atomic.Bool
}

func (c *clientSession) enqueue(m outMsg) bool {
	select {
	case <-c.closed:
		return false
	case c.sendCh <- m:
		return true
	default:
		return false
	}
}

func (c *clientSession) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.rw.Close()
	})
}

// Broker is an isolated MQTT 3.1.1 broker for one project.
type Broker struct {
	mu          sync.RWMutex
	maxClients  int
	retained    RetainedStore
	auth        Authenticator
	onPublish   PublishHandler
	clients     map[string]*clientSession
	retainedMem map[string]RetainedMessage // fallback/cache when RetainedStore is nil
	anonSeq     atomic.Uint64
	closed      atomic.Bool
}

// NewBroker creates a per-project MQTT broker.
func NewBroker(cfg BrokerConfig) *Broker {
	maxC := cfg.MaxClients
	if maxC <= 0 {
		maxC = defaultMaxClients
	}
	b := &Broker{
		maxClients:  maxC,
		retained:    cfg.Retained,
		auth:        cfg.Auth,
		onPublish:   cfg.OnPublish,
		clients:     make(map[string]*clientSession),
		retainedMem: make(map[string]RetainedMessage),
	}
	if b.retained != nil {
		if msgs, err := b.retained.ListRetained(); err == nil {
			for _, m := range msgs {
				b.retainedMem[m.Topic] = m
			}
		}
	}
	return b
}

// UpdateConfig updates dynamic broker settings without dropping existing clients.
func (b *Broker) UpdateConfig(maxClients int, auth Authenticator, onPublish PublishHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if maxClients > 0 {
		b.maxClients = maxClients
	}
	b.auth = auth
	b.onPublish = onPublish
}

// Close disconnects all active clients.
func (b *Broker) Close() {
	b.closed.Store(true)
	b.mu.Lock()
	all := make([]*clientSession, 0, len(b.clients))
	for _, c := range b.clients {
		c.replaced.Store(true) // suppress LWT on server shutdown
		all = append(all, c)
	}
	b.clients = make(map[string]*clientSession)
	b.mu.Unlock()
	for _, c := range all {
		c.close()
	}
}

// Stats returns a snapshot of connected clients and subscriptions.
func (b *Broker) Stats() BrokerStats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ids := make([]string, 0, len(b.clients))
	subs := 0
	for id, c := range b.clients {
		ids = append(ids, id)
		subs += len(c.subs)
	}
	return BrokerStats{
		ConnectedClients: len(ids),
		ClientIDs:        ids,
		Subscriptions:    subs,
	}
}

// HandleConn serves one MQTT 3.1.1 connection until disconnect or context cancel.
func (b *Broker) HandleConn(ctx context.Context, rw io.ReadWriteCloser) {
	defer func() { _ = rw.Close() }()
	if b.closed.Load() {
		return
	}

	br := bufio.NewReader(rw)
	setDeadline(rw, connectHandshakeTimout)
	pkt, err := readPacket(br)
	if err != nil || pkt.ptype != pktConnect {
		return
	}

	cp, code, err := parseConnect(pkt.body)
	if err != nil {
		_ = writePacket(rw, pktConnAck, 0, []byte{0x00, code})
		return
	}

	clientID := cp.clientID
	if clientID == "" {
		clientID = fmt.Sprintf("of-%d", b.anonSeq.Add(1))
	}

	b.mu.RLock()
	authFn := b.auth
	maxC := b.maxClients
	b.mu.RUnlock()

	var authRes AuthResult
	if authFn != nil {
		var ok bool
		authRes, ok = authFn(clientID, cp.username, cp.password)
		if !ok {
			_ = writePacket(rw, pktConnAck, 0, []byte{0x00, connNotAuthorized})
			return
		}
	} else {
		authRes = AuthResult{AllowPub: "#", AllowSub: "#"}
	}

	if cp.willFlag {
		if err := ValidatePublishTopic(cp.willTopic); err != nil {
			_ = writePacket(rw, pktConnAck, 0, []byte{0x00, connNotAuthorized})
			return
		}
	}

	sess := &clientSession{
		id:         clientID,
		deviceName: authRes.DeviceName,
		username:   authRes.Username,
		allowPub:   authRes.AllowPub,
		allowSub:   authRes.AllowSub,
		rw:         rw,
		sendCh:     make(chan outMsg, clientSendQueueSize),
		subs:       make(map[string]byte),
		willFlag:   cp.willFlag,
		willTopic:  cp.willTopic,
		willMsg:    cp.willMsg,
		willQoS:    min(cp.willQoS, 1),
		willRetain: cp.willRetain,
		closed:     make(chan struct{}),
	}

	b.mu.Lock()
	prev := b.clients[clientID]
	if prev == nil && len(b.clients) >= maxC {
		b.mu.Unlock()
		_ = writePacket(rw, pktConnAck, 0, []byte{0x00, connNotAuthorized})
		return
	}
	b.clients[clientID] = sess
	b.mu.Unlock()

	if prev != nil {
		prev.replaced.Store(true)
		prev.close()
	}

	if err := writePacket(rw, pktConnAck, 0, []byte{0x00, connAccepted}); err != nil {
		b.removeSession(sess)
		sess.close()
		return
	}

	go func() {
		select {
		case <-ctx.Done():
			sess.close()
		case <-sess.closed:
		}
	}()

	go b.writeLoop(sess)

	var keepAlive time.Duration
	if cp.keepAlive > 0 {
		keepAlive = time.Duration(cp.keepAlive) * time.Second * 3 / 2
	}

	for {
		setDeadline(rw, keepAlive)
		p, err := readPacket(br)
		if err != nil {
			break
		}
		switch p.ptype {
		case pktPublish:
			pub, err := parsePublish(p.flags, p.body)
			if err != nil {
				goto done
			}
			if err := ValidatePublishTopic(pub.topic); err != nil {
				goto done
			}
			if !MatchAny(sess.allowPub, pub.topic) {
				if pub.qos == 1 {
					var ack [2]byte
					binary.BigEndian.PutUint16(ack[:], pub.packetID)
					sess.enqueue(outMsg{ptype: pktPubAck, body: ack[:]})
				}
				continue
			}
			if pub.qos == 1 {
				var ack [2]byte
				binary.BigEndian.PutUint16(ack[:], pub.packetID)
				sess.enqueue(outMsg{ptype: pktPubAck, body: ack[:]})
			}
			b.dispatchPublish(PublishEvent{
				ClientID:   sess.id,
				DeviceName: sess.deviceName,
				Username:   sess.username,
				Topic:      pub.topic,
				Payload:    pub.payload,
				QoS:        pub.qos,
				Retain:     pub.retain,
			}, true)

		case pktSubscribe:
			pktID, items, err := parseSubscribe(p.body)
			if err != nil {
				goto done
			}
			codes := make([]byte, len(items))
			var newlyAdded []subItem
			b.mu.Lock()
			for i, it := range items {
				if ValidateTopicFilter(it.filter) != nil || !b.canSubscribe(sess.allowSub, it.filter) {
					codes[i] = 0x80 // Failure
					continue
				}
				granted := min(it.qos, 1)
				sess.subs[it.filter] = granted
				codes[i] = granted
				newlyAdded = append(newlyAdded, subItem{filter: it.filter, qos: granted})
			}
			retainedList := make([]RetainedMessage, 0, len(b.retainedMem))
			for _, rm := range b.retainedMem {
				retainedList = append(retainedList, rm)
			}
			b.mu.Unlock()

			ackBody := make([]byte, 2+len(codes))
			binary.BigEndian.PutUint16(ackBody[:2], pktID)
			copy(ackBody[2:], codes)
			sess.enqueue(outMsg{ptype: pktSubAck, body: ackBody})

			for _, rm := range retainedList {
				for _, sub := range newlyAdded {
					if MatchTopic(sub.filter, rm.Topic) {
						qos := min(sub.qos, rm.QoS)
						var outID uint16
						if qos > 0 {
							outID = uint16(sess.nextPktID.Add(1))
						}
						body, flags := encodePublish(rm.Topic, rm.Payload, qos, true, outID)
						sess.enqueue(outMsg{ptype: pktPublish, flags: flags, body: body})
						break
					}
				}
			}

		case pktUnsubscribe:
			pktID, filters, err := parseUnsubscribe(p.body)
			if err != nil {
				goto done
			}
			b.mu.Lock()
			for _, f := range filters {
				delete(sess.subs, f)
			}
			b.mu.Unlock()
			var ack [2]byte
			binary.BigEndian.PutUint16(ack[:], pktID)
			sess.enqueue(outMsg{ptype: pktUnsubAck, body: ack[:]})

		case pktPingReq:
			sess.enqueue(outMsg{ptype: pktPingResp})

		case pktDisconnect:
			sess.cleanExit.Store(true)
			goto done

		default:
			goto done
		}
	}

done:
	b.removeSession(sess)
	sess.close()
	if sess.willFlag && !sess.cleanExit.Load() && !sess.replaced.Load() {
		if MatchAny(sess.allowPub, sess.willTopic) {
			b.dispatchPublish(PublishEvent{
				ClientID:   sess.id,
				DeviceName: sess.deviceName,
				Username:   sess.username,
				Topic:      sess.willTopic,
				Payload:    sess.willMsg,
				QoS:        sess.willQoS,
				Retain:     sess.willRetain,
			}, true)
		}
	}
}

// Publish delivers a programmatic message (from ctx.mqtt.publish or simulate)
// to matching subscribers and updates retained storage if retain is true.
func (b *Broker) Publish(topic string, payload []byte, qos byte, retain bool) error {
	if err := ValidatePublishTopic(topic); err != nil {
		return err
	}
	if qos > 1 {
		qos = 1
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	b.dispatchPublish(PublishEvent{
		ClientID: "server",
		Topic:    topic,
		Payload:  cp,
		QoS:      qos,
		Retain:   retain,
	}, true)
	return nil
}

func (b *Broker) dispatchPublish(ev PublishEvent, fireHook bool) {
	if ev.Retain {
		b.mu.Lock()
		if len(ev.Payload) == 0 {
			delete(b.retainedMem, ev.Topic)
		} else {
			b.retainedMem[ev.Topic] = RetainedMessage{
				Topic:   ev.Topic,
				Payload: ev.Payload,
				QoS:     ev.QoS,
			}
		}
		retStore := b.retained
		b.mu.Unlock()
		if retStore != nil {
			if len(ev.Payload) == 0 {
				_ = retStore.DeleteRetained(ev.Topic)
			} else {
				_ = retStore.SetRetained(ev.Topic, ev.Payload, ev.QoS)
			}
		}
	}

	b.mu.RLock()
	targets := make([]struct {
		sess *clientSession
		qos  byte
	}, 0, len(b.clients))
	for _, c := range b.clients {
		var matched bool
		var maxSubQoS byte
		for f, subQoS := range c.subs {
			if MatchTopic(f, ev.Topic) {
				if !matched || subQoS > maxSubQoS {
					maxSubQoS = subQoS
				}
				matched = true
			}
		}
		if matched {
			targets = append(targets, struct {
				sess *clientSession
				qos  byte
			}{sess: c, qos: min(ev.QoS, maxSubQoS)})
		}
	}
	hook := b.onPublish
	b.mu.RUnlock()

	for _, t := range targets {
		var pktID uint16
		if t.qos > 0 {
			pktID = uint16(t.sess.nextPktID.Add(1))
		}
		body, flags := encodePublish(ev.Topic, ev.Payload, t.qos, false, pktID)
		t.sess.enqueue(outMsg{ptype: pktPublish, flags: flags, body: body})
	}

	if fireHook && hook != nil {
		go hook(ev)
	}
}

func (b *Broker) canSubscribe(allowSub, filter string) bool {
	if allowSub == "#" || MatchAny(allowSub, filter) {
		return true
	}
	// Exact-prefix or concrete filter check against ACLs.
	for _, raw := range splitCSV(allowSub) {
		if raw == filter || MatchTopic(raw, filter) {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trimSpace(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func (b *Broker) removeSession(sess *clientSession) {
	b.mu.Lock()
	if b.clients[sess.id] == sess {
		delete(b.clients, sess.id)
	}
	b.mu.Unlock()
}

func (b *Broker) writeLoop(sess *clientSession) {
	for {
		select {
		case <-sess.closed:
			return
		case m := <-sess.sendCh:
			if err := writePacket(sess.rw, m.ptype, m.flags, m.body); err != nil {
				sess.close()
				return
			}
		}
	}
}

func setDeadline(rw io.ReadWriteCloser, d time.Duration) {
	type deadliner interface {
		SetReadDeadline(time.Time) error
	}
	dl, ok := rw.(deadliner)
	if !ok {
		return
	}
	if d <= 0 {
		_ = dl.SetReadDeadline(time.Time{})
		return
	}
	_ = dl.SetReadDeadline(time.Now().Add(d))
}

// ServeTCP accepts raw MQTT TCP connections on ln until ctx is cancelled.
func ServeTCP(ctx context.Context, ln net.Listener, handler func(context.Context, net.Conn)) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go handler(ctx, conn)
	}
}
