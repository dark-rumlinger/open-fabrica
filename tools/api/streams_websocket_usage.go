package api

// buildWSCreateUsage renders the model-facing usage doc for the realtime
// create/update tools from a resolved wsUsageSpec. Pure string assembly, split
// out of streams_websocket.go to keep the endpoint engine readable.

import (
	"fmt"

	"openfabrica/contracts"
)

// wsUsageSpec carries the resolved endpoint settings buildWSCreateUsage renders into
// the create/update usage doc, as a struct rather than an error-prone 17-arg signature.
type wsUsageSpec struct {
	Path         string
	RequiresAuth bool
	HelperOK     bool
	TenantColumn string
	RoomColumn   string
	Matchmaking  bool
	GroupSize    int
	MaxClients   int
	Signaling    bool
	E2EE         bool
	RoomGated    bool
	Ephemeral    bool
	WriteToTable string
	MessageTTL   int
	TickHz       int
	RateLimit    int
	MaxMsgSize   int
	// Authoritative selects the delivery model the block documents. It is not implied
	// by TickHz: on an authoritative endpoint tick_hz drives @tick into reduce() while
	// the actor pushes {_type:'state'} views, so the snapshot lane must not be shown.
	Authoritative bool
}

func buildWSCreateUsage(spec wsUsageSpec) string {
	path := spec.Path
	requiresAuth := spec.RequiresAuth
	helperOK := spec.HelperOK
	tenantColumn := spec.TenantColumn
	roomColumn := spec.RoomColumn
	matchmaking := spec.Matchmaking
	groupSize := spec.GroupSize
	maxClients := spec.MaxClients
	signaling := spec.Signaling
	e2ee := spec.E2EE
	roomGated := spec.RoomGated
	ephemeral := spec.Ephemeral
	writeToTable := spec.WriteToTable
	messageTTL := spec.MessageTTL
	tickHz := spec.TickHz
	rateLimit := spec.RateLimit
	maxMsgSize := spec.MaxMsgSize
	authoritative := spec.Authoritative
	// The client API surface and the two-lane send/event rule are taught by the
	// realtime_signaling guide, which is already in the system prompt by the time this
	// result is read (the plan declared this tool, so the guide was prefetched with it).
	// Restating them here paid for the same prose twice and put a second copy on a
	// drift path nothing scans. What belongs here is what the guide CANNOT know: this
	// endpoint's own path, mode and settings.
	usage := fmt.Sprintf(`CONNECTION (import the platform-managed realtime helper by specifier - do NOT edit/recreate it or hand-wire the relay):
  import { connect } from 'ofnet';
  const net = connect("%s", { room: roomId, meta: { name } });
  // room scopes the channel (omit it for one global channel); meta is the optional
  // profile that rides presence.
  // The full client surface - onPeers/peers/peerMeta, on/onMessage, event vs send,
  // to, onMatched, myId, ws - is in the realtime_signaling guide's core, already in
  // your prompt. Recall it with manage_guides action=recall slug=realtime_signaling
  // if it is not.`, path)

	if requiresAuth {
		usage += `

AUTH (this endpoint has requires_auth=true):
  connect() auto-carries the logged-in user's token (localStorage['auth_token'],
  the same source the 'auth' helper's apiFetch uses) as ?token= - no manual token
  threading; just be signed in (gate the page with requireAuth, imported from 'auth').
  A raw new WebSocket() can NOT
  authenticate (a browser socket cannot send an Authorization header) and is refused
  with HTTP 401 before the upgrade - NEVER open a raw socket to this endpoint.`
	} else {
		usage += fmt.Sprintf(`
  (This endpoint is public, so a raw socket also works:
     const ws = new WebSocket((location.protocol==='https:'?'wss:':'ws:') + '//' + location.host + '/api/%s/ws?room=' + roomId);
   but prefer connect() from 'ofnet' - it seeds presence, keeps a stable identity
   across refreshes, and handles matchmaking room switches for you.)`, path)
	}

	if !helperOK {
		usage += `

  NOTE: emitting js/ofnet.js FAILED on this call, so the 'ofnet' module may be missing.
  Re-run this endpoint's create (idempotent) via its realtime tool (manage_relay /
  manage_tick / manage_authoritative / manage_matchmaking / manage_signaling) to
  re-emit it before importing from 'ofnet'.`
	}

	// The system frames, presence rule and two lanes are the realtime-frames contract,
	// already in the system prompt by the time this result is read (creating a websocket
	// endpoint pulls the realtime_signaling guide, whose core transcludes it). Point at
	// it instead and spend the result on what is specific to THIS endpoint.
	usage += `

FRAMES: the welcome/join/leave shapes, presence via net.onPeers, the snapshot frame,
and the two send lanes are the realtime-frames contract in your realtime guide - build
from there, it is already in front of you.`

	if tenantColumn != "" {
		usage += fmt.Sprintf(`

MULTI-TENANT ISOLATION (this endpoint has tenant_column=%q):
  Connections must present a tenant-scoped JWT (call /tenant/switch first if
  the token isn't already tenant-scoped). The server folds tenant_id into the
  room key, so two tenants can both join ?room=lobby and remain fully isolated.
  Persisted messages auto-stamp the %q column from the JWT, never from the wire
  payload. Any client value for that column is ignored.`, tenantColumn, tenantColumn)
	}

	if roomColumn != "" {
		usage += fmt.Sprintf(`

DYNAMIC ROOM SWITCHING (this endpoint has room_column=%q):
  Send a message containing the %q field and the server moves you to that room
  without reconnecting. The new room sends you a fresh welcome + join/leave events.
    net.switchRoom("new_room_name", %q);   // sends { %s: "new_room_name" }
  Use this for lobby -> match handoff instead of closing and reopening the socket.
  The switch field MUST be exactly %q; if this endpoint also persists messages that
  is the same DB column the room is stored in (the relay scopes ephemeral prune on
  it), so a different field name silently breaks switching and/or cleanup. NOTE:
  room_gated endpoints fix the room at connect - live switching is disabled there.`, roomColumn, roomColumn, roomColumn, roomColumn, roomColumn)
	}

	if matchmaking {
		usage += fmt.Sprintf(`

MATCHMAKING (this is a manage_matchmaking lobby, group_size=%d):
  The SERVER pairs peers - do NOT hand-roll matchmaking in client JS. Each client
  connects to the lobby room and waits; once %d unmatched peers are present the
  server mints ONE shared match room, assigns roles, and sends each peer:
    { "_type":"matched", "_room":"m-<random id>", "room_field":"<room_column>", "role":"host"|"player", "index":I, "players":[ids] }
  connect() does the whole dance: on "matched" it auto-switches you into the assigned
  room, so the entire client is
    import { connect } from 'ofnet';
    connect("%s", { room: "lobby" }).onMatched((m) => { startGame(m.role); });
  Room id and role are server-owned, so peers always land in the SAME room and can
  never both-challenge into a deadlock. Do NOT derive a room id from client ids,
  mint your own, or invent a JOIN/announce frame - the server already paired you.
  The match room is capped at group_size: anyone else who learns its id joins as a
  read-only spectator.
  NOTE: a matchmaking endpoint can NOT also be authoritative (set_reducer on it is
  refused). For an auto-matched server-authoritative game, pair here, then connect the
  matched peers to a SEPARATE manage_authoritative endpoint (create it with
  ephemeral=true and max_clients=%d) using the assigned id:
    connect("%s", { room: "lobby" }).onMatched((m) => {
      const game = connect("<game endpoint>", { room: m._room });
      game.onState(render);
    });`, groupSize, groupSize, path, groupSize, path)
	}

	if maxClients > 0 {
		usage += fmt.Sprintf(`

CAPACITY (this endpoint has max_clients=%d):
  A room holds at most %d participants. The relay enforces this atomically on join
  (a join race can't overfill). The (%d+1)th and later joiners are admitted as
  read-only SPECTATORS: they receive every broadcast/snapshot but their inbound
  frames are dropped. The welcome frame tells each client which it is:
    { "_type":"welcome", "_role":"player"|"spectator", "_capacity":%d, ... }
  Gate input on it (the ofnet helper exposes net.spectator). Don't try to enforce a player cap
  in client JS - a simultaneous join would slip past it; the server is the only
  place it holds.`, maxClients, maxClients, maxClients, maxClients)
	}

	// Section ordering: the flag-specific contracts (signaling, e2ee, room_gated,
	// ephemeral) describe what this call just enabled, so they are emitted BEFORE the
	// generic broadcasts/lobby boilerplate - a head-truncation must never drop the
	// just-enabled contract while keeping generic prose.
	if signaling {
		usage += `

WEBRTC SIGNALING MODE:
  This endpoint is a WebRTC signaling relay. The hub only forwards JSON SDP/ICE
  frames between peers (use _to for point-to-point); media flows peer-to-peer
  via RTCPeerConnection and never touches the server. Signaling mode bumps
  max_msg_size to 64KB and disables tick coalescing so SDP/ICE relay is immediate.

  DO NOT invent your own signaling protocol or copy a generic WebRTC tutorial.
  The canonical client (role selection from welcome, ICE buffering, TURN
  configuration, browser-native RTCPeerConnection only - no simple-peer / peerjs
  wrappers) is documented in the webrtc_signaling guide and is
  auto-inlined into your BUILD context whenever your plan contains a
  manage_signaling endpoint. Call manage_guides action=recall slug=webrtc_signaling
  if you need to re-read it. Following that contract verbatim is what makes
  "two people can see each other" actually work; mixing wrappers or skipping
  ICE buffering / TURN are the three ways calls silently hang at "waiting for peer".`
	}

	if e2ee {
		usage += `

END-TO-END ENCRYPTION MODE (this endpoint has e2ee=true):
  The platform has emitted the crypto helper js/e2ee.js as a platform-managed ES
  module - do NOT author, recreate, or delete it. Import it by specifier ('e2ee')
  and never call crypto.subtle directly; manage_guides action=recall slug=e2ee has
  the full usage contract. The relay BACKSTOPS the contract, it does not prove it:
  a frame carrying a readable body field (text/content/body/message/msg/plaintext/
  caption/note) is dropped, and only the {iv,ct} envelope is persisted to
  write_to_table. That check is a body-field denylist, so a frame that puts secrets
  under any OTHER key (a title, a filename, a nickname) relays IN CLEARTEXT and the relay will not stop
  it. Put every secret through encrypt() and send nothing else. Bodyless control
  frames pass by design: e2ee-pub (key exchange) and WebRTC signaling
  (offer/answer/ice/hello/bye).

  The wiring (start / handleSignal / encrypt / decrypt / onReady / verifyCode), the
  modes (two-party, mode:'group' for 3+, ratchet:true for per-message forward secrecy,
  archive:true for readable history), and the fail-closed UI rules are the e2ee guide's
  contract. The wire contract follows below; for the full session API run
  manage_guides action=recall slug=e2ee BEFORE writing the client (the guide is only in
  your prompt if the plan listed it). Do not improvise around it - a composer enabled
  before onReady, or a key derivation of your own, ships a room that looks encrypted
  and is not.

` + contracts.MustRender(contracts.IDE2EEWire)

		if signaling {
			usage += `

  PERSISTING CHAT: this is a manage_signaling endpoint - it forwards the WebRTC
  handshake and has no write_to_table, so there is nothing to persist here (SDP/ICE
  are not history). If the app ALSO keeps an encrypted text chat, that is a
  manage_relay endpoint with e2ee + write_to_table + room_column, created alongside
  this one.`
		} else {
			usage += `

  PERSISTING CHAT: if the chat keeps history, set write_to_table + room_column on
  THIS endpoint and give that table iv/ct/a columns. The relay stores only the
  ciphertext envelope for you. Do NOT stand up a separate REST messages table you
  then have to clean up by hand - that is the trap that makes builds spin.
  You MUST also open the session with archive: true. The default message key is a
  per-connection ephemeral that is gone on reload, so rows written without it are
  ciphertext nobody - not even their sender - can ever reopen. archive is the ONLY
  persistable lane: NOT ratchet, whose per-message keys are destroyed after use, so
  the relay refuses to persist ratchet frames (it warns instead). See manage_guides
  action=recall slug=e2ee section=readable-history-archive.`
		}
	}

	if roomGated {
		usage += `

ROOM-GATED ENTRY (this endpoint has room_gated=true):
  Knowing the room name is NOT enough to join. The relay requires a per-room
  access token on the connect URL: ...?room=<name>&join=<token>. This token is
  SEPARATE from any E2EE password (which must stay client-only in the URL
  fragment and never reach the server). Flow for a 2-party call/room:

    1. The FIRST peer connects with ?room=<name> and no join token. The relay
       mints the token and hands it back in the welcome frame:
         { "_type":"welcome", "_clientId":"...", "_joinToken":"<token>", ... }
    2. That peer builds the shareable invite link carrying BOTH the room and the
       token in the URL, e.g. /room/<name>?join=<token>#pw=<password>
       (join token in the query/path = an access capability; password in the
       fragment = the E2EE key, never sent to the server).
    3. The SECOND peer opens the link; its room page reads ?join=<token> and
       passes it to connect(path, { room, join: token }). A missing/wrong
       token is refused with HTTP 401 BEFORE the socket upgrades, so an uninvited
       peer can never become the second participant or pull the peer-to-peer media.

  The token row lives only while the room is occupied; it is pruned when the
  room empties, so the link is single-session. Do NOT store the join token in a
  table or send it through any other endpoint - read _joinToken from welcome and
  put it in the link, nothing more.`
	}

	if ephemeral && authoritative {
		usage += `

EPHEMERAL ROOMS (this endpoint has ephemeral=true):
  When a room empties (the last player leaves), its persisted reducer state is
  dropped, so the next game in that room id starts again from @init. Per-match rooms
  want this; a persistent world (state that must survive everyone leaving) does not.`
	} else if ephemeral {
		if writeToTable != "" && roomColumn != "" {
			usage += `

EPHEMERAL ROOMS (this endpoint has ephemeral=true):
  When a room empties (the last peer leaves), the relay AUTOMATICALLY deletes this
  endpoint's persisted rows for that room. "Nothing stored after the session ends"
  is handled server-side. Do NOT delete rows from the client on call-end/unload,
  and do NOT call manage_data at runtime (it is a build-time tool). For hang-up,
  just send a 'bye' frame and close the socket - the relay prunes and tears down.`
		} else {
			usage += `

EPHEMERAL ROOMS (this endpoint has ephemeral=true) - NOT YET ACTIVE:
  Auto-prune needs BOTH write_to_table and room_column so the relay can scope the
  delete to the room. Set them (with iv/ct columns for e2ee) and persisted rows are
  cleaned up when the room empties - no client-side delete needed.`
		}
	}

	if messageTTL > 0 {
		usage += fmt.Sprintf(`

DISAPPEARING MESSAGES (this endpoint has message_ttl_seconds=%d):
  Persisted rows older than %d seconds are swept server-side (on a connect, and when
  a room empties), so stored history self-expires - no client-driven delete. Needs
  write_to_table. For a message to also vanish from an ALREADY-OPEN window, hide it
  client-side once (Date.now()/1000 - sentAtEpoch) exceeds the TTL: the server owns
  what is PERSISTED, the client owns what an open session keeps SHOWING.`, messageTTL, messageTTL)
	}

	if authoritative {
		usage += `

AUTHORITATIVE MODE (a server reducer owns this endpoint's state):
  net.action() frames are consumed by the room actor: applied through reduce(),
  never relayed. Every OTHER frame still broadcasts to peers exactly like a relay,
  so chat alongside the game keeps working on this one endpoint. The endpoint never
  emits a 'snapshot'; state reaches clients as its own push:

  net.onState((state) => render(state));   // pushed after every change AND on join
  net.action("play", { cell: 4 });         // reduce() sees {type:'play', cell:4, _by:<you>}

  The event shape (payload spread at the top level) and the render-from-onState-only
  rule are the realtime-frames contract in your realtime guide, already in front of you.`
		if tickHz > 0 {
			usage += fmt.Sprintf(`
  tick_hz=%d here drives a {type:'@tick', dt} event into reduce() %d times/sec so the
  reducer advances the sim server-side. It is a SIM RATE, not the coalescing tick:
  this endpoint does not coalesce net.send or emit snapshots.`, tickHz, tickHz)
		}
		// The reducer contract rides the create result because that is the moment before
		// the reducer gets written. Transcluded, so it cannot drift from its single home.
		// MustRender + the ID constant makes a renamed contract break here rather than
		// quietly ship a create result that teaches the reducer nothing.
		usage += "\n\n" + contracts.MustRender(contracts.IDRoomReducer)
	}

	if tickHz > 0 && !authoritative {
		usage += fmt.Sprintf(`

TICK MODE (this endpoint has tick_hz=%d):
  Every player streams state as fast as they like and the server collapses it to %d Hz
  of room-wide snapshots; only the latest pre-tick frame survives. Direct messages
  (net.to) and binary frames bypass the tick and are forwarded immediately. The
  snapshot frame, the two lanes, and what write_to_table does with each are in the
  realtime_signaling guide.`, tickHz, tickHz)
	}
	if rateLimit > 0 {
		usage += fmt.Sprintf("\n\nRATE LIMIT: %d inbound text messages/sec per client. Excess is dropped (the sender gets one _type:'error' frame, so subscribe to net.on('error')).", rateLimit)
		if e2ee {
			usage += " Binary frames are rejected on this e2ee endpoint (send encrypted blobs through the 'e2ee' session's encryptBlob(), which returns {meta, chunks:[base64]} to carry as JSON - each chunk is ONE opaque base64 string, never an {iv,ct} object)."
		} else {
			usage += " Binary frames are not rate-limited."
		}
	}
	if maxMsgSize > 0 {
		usage += fmt.Sprintf("\nMAX MSG SIZE: %d bytes per frame. Oversize triggers an _type:\"error\" reply; the connection stays open.", maxMsgSize)
	}

	// Generic relay patterns last: useful background, but never as important as the
	// enabled-flag contracts above, so these are the safe thing to lose to a cut.
	usage += fmt.Sprintf(`

SERVER-INITIATED BROADCASTS (no client holds the socket):
  Use manage_triggers with trigger_type "ws_broadcast" on data.insert / data.update / data.delete
  to push messages from server logic (REST calls, jobs, schedulers) into a room.
  Configure endpoint_path, room (supports {{templateVars}} from the event), and message.
  For tenanted endpoints (tenant_column set) also set tenant_id in the trigger config,
  typically templated as {{tenant_id}} from the inserted row; omitting it lands in the
  anonymous bucket where tenanted clients never connect.

ROOM LIST / PRESENCE COUNTS (no extra wiring):
  GET /api/%s/rooms -> [{room, count}] for every active room on this endpoint;
  add ?room=<name> for {room, clients:[ids], count} of one room (a requires_auth
  endpoint needs the Authorization: Bearer token on this call). Use it to show
  open games / lobby player counts - do NOT build your own presence-tracking
  protocol for that.

LOBBIES:
  For "find me an opponent / auto-match", create a manage_matchmaking endpoint (the
  server pairs peers and hands each a shared match room) rather than pairing in client
  JS. A hand-rolled lobby has two fatal traps of its own - both peers must land on ONE
  room id, and exactly one side must initiate - which the realtime_signaling guide's
  "Custom lobby handoff" section covers: recall it if server matchmaking does not fit.`, path)
	return usage
}
