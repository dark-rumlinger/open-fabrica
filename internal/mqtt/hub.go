package mqtt

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"sync"

	"github.com/coder/websocket"
)

// ProjectHooks connects a per-project MQTT Broker to Open Fabrica's persistence,
// auth, and rule-ingest layers without creating package import cycles.
type ProjectHooks struct {
	Enabled        func(projectID int) bool
	LoadConfig     func(projectID int) (BrokerConfig, error)
	ResolveTCPConn func(username, password string) (projectID int, ok bool)
}

// Hub manages isolated MQTT brokers per project and routes both WebSocket
// (/mqtt) and optional raw TCP (:1883) connections to the right project broker.
type Hub struct {
	mu      sync.Mutex
	hooks   ProjectHooks
	brokers map[int]*Broker
}

// NewHub creates a multi-project MQTT hub.
func NewHub(hooks ProjectHooks) *Hub {
	return &Hub{
		hooks:   hooks,
		brokers: make(map[int]*Broker),
	}
}

// BrokerFor returns (or lazily initializes) the broker for projectID, or nil if
// MQTT is disabled for that project.
func (h *Hub) BrokerFor(projectID int) (*Broker, error) {
	if h.hooks.Enabled != nil && !h.hooks.Enabled(projectID) {
		return nil, nil
	}
	h.mu.Lock()
	if b := h.brokers[projectID]; b != nil && !b.closed.Load() {
		h.mu.Unlock()
		return b, nil
	}
	h.mu.Unlock()

	if h.hooks.LoadConfig == nil {
		b := NewBroker(BrokerConfig{})
		h.mu.Lock()
		h.brokers[projectID] = b
		h.mu.Unlock()
		return b, nil
	}

	cfg, err := h.hooks.LoadConfig(projectID)
	if err != nil {
		return nil, err
	}
	b := NewBroker(cfg)

	h.mu.Lock()
	if existing := h.brokers[projectID]; existing != nil && !existing.closed.Load() {
		h.mu.Unlock()
		b.Close()
		return existing, nil
	}
	h.brokers[projectID] = b
	h.mu.Unlock()
	return b, nil
}

// Reload refreshes an active project's broker config (or closes it if disabled).
// If the project is enabled and has outbound bridges configured, Reload also
// starts the broker immediately so bridges connect without waiting for a client.
func (h *Hub) Reload(projectID int) {
	if h.hooks.Enabled != nil && !h.hooks.Enabled(projectID) {
		h.mu.Lock()
		b := h.brokers[projectID]
		delete(h.brokers, projectID)
		h.mu.Unlock()
		if b != nil {
			b.Close()
		}
		return
	}
	if h.hooks.LoadConfig == nil {
		return
	}
	cfg, err := h.hooks.LoadConfig(projectID)
	if err != nil {
		return
	}
	h.mu.Lock()
	b := h.brokers[projectID]
	h.mu.Unlock()
	if b == nil {
		if len(cfg.Bridges) > 0 || cfg.HasWatchdogs {
			_, _ = h.BrokerFor(projectID)
		}
		return
	}
	b.UpdateConfig(cfg.MaxClients, cfg.Auth, cfg.OnPublish, cfg.OnConnect, cfg.OnDisconnect, cfg.Bridges)
}

// StartBridgedProjects starts brokers for any enabled project in projectIDs that
// has at least one configured outbound bridge, so bridges reconnect automatically
// after server startup.
func (h *Hub) StartBridgedProjects(projectIDs []int) {
	for _, pid := range projectIDs {
		h.Reload(pid)
	}
}

// StatsFor returns live broker stats for projectID if a broker is currently active.
func (h *Hub) StatsFor(projectID int) (BrokerStats, bool) {
	h.mu.Lock()
	b := h.brokers[projectID]
	h.mu.Unlock()
	if b == nil || b.closed.Load() {
		return BrokerStats{}, false
	}
	return b.Stats(), true
}

// CloseProject closes and removes the active broker for a single project (e.g. on delete).
func (h *Hub) CloseProject(projectID int) {
	h.mu.Lock()
	b := h.brokers[projectID]
	delete(h.brokers, projectID)
	h.mu.Unlock()
	if b != nil {
		b.Close()
	}
}

// CloseAll shuts down every active project broker.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	all := make([]*Broker, 0, len(h.brokers))
	for _, b := range h.brokers {
		all = append(all, b)
	}
	h.brokers = make(map[int]*Broker)
	h.mu.Unlock()
	for _, b := range all {
		b.Close()
	}
}

// HandleTCPConn peeks the initial MQTT CONNECT packet on a raw TCP connection to
// determine which project owns the credentials, then hands the buffered stream
// to that project's Broker.
func (h *Hub) HandleTCPConn(ctx context.Context, conn net.Conn) {
	setDeadline(conn, connectHandshakeTimout)
	br := bufio.NewReader(conn)
	pkt, err := readPacket(br)
	if err != nil || pkt.ptype != pktConnect {
		_ = conn.Close()
		return
	}
	cp, code, err := parseConnect(pkt.body)
	if err != nil {
		_ = writePacket(conn, pktConnAck, 0, []byte{0x00, code})
		_ = conn.Close()
		return
	}
	if h.hooks.ResolveTCPConn == nil {
		_ = writePacket(conn, pktConnAck, 0, []byte{0x00, connNotAuthorized})
		_ = conn.Close()
		return
	}
	projectID, ok := h.hooks.ResolveTCPConn(cp.username, cp.password)
	if !ok {
		_ = writePacket(conn, pktConnAck, 0, []byte{0x00, connBadUserOrPass})
		_ = conn.Close()
		return
	}
	b, err := h.BrokerFor(projectID)
	if err != nil || b == nil {
		_ = writePacket(conn, pktConnAck, 0, []byte{0x00, connNotAuthorized})
		_ = conn.Close()
		return
	}

	// Re-synthesize the raw CONNECT packet so Broker.HandleConn runs one uniform path.
	var hdr [5]byte
	hdr[0] = (pktConnect << 4)
	n := encodeVarInt(hdr[1:], len(pkt.body))
	var pre bytes.Buffer
	pre.Write(hdr[:1+n])
	pre.Write(pkt.body)

	b.HandleConn(ctx, &prefixedConn{
		Reader: io.MultiReader(&pre, br),
		Conn:   conn,
	})
}

type prefixedConn struct {
	io.Reader
	net.Conn
}

func (p *prefixedConn) Read(b []byte) (int, error) {
	return p.Reader.Read(b)
}

// WSConn adapts a binary WebSocket connection to io.ReadWriteCloser for MQTT-over-WS.
type WSConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	ws     *websocket.Conn
	mu     sync.Mutex
	cur    io.Reader
}

// NewWSConn wraps a coder/websocket Conn as an io.ReadWriteCloser.
func NewWSConn(ctx context.Context, ws *websocket.Conn) *WSConn {
	cctx, cancel := context.WithCancel(ctx)
	return &WSConn{
		ctx:    cctx,
		cancel: cancel,
		ws:     ws,
	}
}

func (w *WSConn) Read(p []byte) (int, error) {
	for {
		if w.cur == nil {
			_, r, err := w.ws.Reader(w.ctx)
			if err != nil {
				return 0, err
			}
			w.cur = r
		}
		n, err := w.cur.Read(p)
		if err == io.EOF {
			w.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (w *WSConn) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ws.Write(w.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *WSConn) Close() error {
	w.cancel()
	return w.ws.Close(websocket.StatusNormalClosure, "")
}
