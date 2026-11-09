package api

// Endpoint default values and bounds. Centralized here so the create and update
// handlers share one source of truth instead of repeating magic numbers, and so
// the per-mode WebSocket defaults can't silently diverge between the two paths.

const (
	// defaultAPIRateLimit is the per-IP requests/minute applied to a REST CRUD
	// endpoint when the caller doesn't set one.
	defaultAPIRateLimit = 60

	// defaultFunctionRateLimit is the per-IP requests/minute applied to a
	// function endpoint when the caller doesn't set one. The create handler
	// inserts the column explicitly, so this - not the column DEFAULT - is what
	// an omitted rate_limit resolves to. It must stay non-zero: the limiter
	// reads its argument as a ceiling, and 0 rejects every request.
	defaultFunctionRateLimit = 60

	// maxFunctionTimeoutMS caps a function endpoint's timeout_ms. The public
	// server's WriteTimeout (server/server.go) cuts the client off at 120s, so a
	// larger budget only pins a goroutine on a request nobody is still reading.
	maxFunctionTimeoutMS = 120000
)

const (
	// maxTickHz caps a WebSocket endpoint's server tick rate - both manage_tick
	// (relay-tick snapshots) and an authoritative endpoint's @tick simulation.
	maxTickHz = 120

	// defaultWSInboundRate is the per-client inbound message/sec cap applied
	// when ws_inbound_rate is unset.
	defaultWSInboundRate = 120

	// defaultChatMaxMsg / defaultSignalMaxMsg are the per-frame byte caps for the
	// two WebSocket modes. Signaling carries SDP offers with many ICE candidates
	// (10-40KB), so it gets a larger default.
	defaultChatMaxMsg   = 16 * 1024 // 16 KB
	defaultSignalMaxMsg = 64 * 1024 // 64 KB

	// maxMaxMsgSize is the hard ceiling on max_msg_size. The relay buffers a full
	// frame per read (server/public/stream.go SetReadLimit), so an unbounded value
	// is a per-connection memory hazard. 1 MiB comfortably fits SDP and e2ee blob
	// chunks; bulk transfers belong on a WebRTC DataChannel, not relay frames.
	maxMaxMsgSize = 1 << 20 // 1 MiB

	// defaultRoomColumn is the room-scope column matchmaking falls back to when
	// the caller doesn't name one (matched peers switch into the assigned room).
	defaultRoomColumn = "room"

	// group_size bounds for a matchmaking endpoint.
	minGroupSize     = 2
	maxGroupSize     = 16
	defaultGroupSize = 2

	// maxMessageTTLSecs caps the disappearing-messages TTL at one year.
	maxMessageTTLSecs = 31536000
)
