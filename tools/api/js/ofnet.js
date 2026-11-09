/* Open Fabrica realtime client helper.
   PLATFORM-MANAGED: the platform owns this file and your pages load it at
   runtime - do not edit, recreate, or delete it. Import it by specifier:
     import { connect, roomCode } from 'ofnet';
   Wraps the /api/<path>/ws relay so a page never re-implements it:
     - onPeers(cb): the room roster (id + meta), delivered whole on join AND on
       first connect - so a joiner sees everyone already present, not just
       whoever arrives after it.
     - send(obj):  your continuous state (position, cursor). Latest-wins: on a
       tick endpoint the server coalesces it, so sending many times a second
       costs one broadcast per tick. Read peers via on('snapshot').
     - event(obj): a discrete message that must arrive intact (chat, an emote).
       Never coalesced, so movement (send) and chat (event) share ONE endpoint.
     - server-side matchmaking (auto-joins the assigned room).
     - auto-reconnect: a dropped socket re-dials the same room with the same
       identity (onReconnect fires when it is back); pass {reconnect:false} to
       opt out.
   See the realtime_signaling guide. */
// AUTH: connect() auto-carries the logged-in user's token (localStorage
// ['auth_token'], the same source the 'auth' helper's apiFetch uses) as ?token= on
// the connect URL, so a requires_auth endpoint works for any signed-in player with
// no manual token threading. Pass opts.token to override.
const OFNet = (function () {
  // Close code the server sends when a newer connection with the same client id took
  // this one's slot. Never auto-reconnect on it: two sockets sharing one id would take
  // the slot back from each other forever.
  var CLOSE_SUPERSEDED = 4001;
  var MAX_QUEUE = 64;

  function connect(path, opts) {
    opts = opts || {};
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    // Auth: a browser WebSocket cannot set an Authorization header, so the token
    // rides as ?token= (the server accepts that header-less form). Mirror
    // the 'auth' helper's apiFetch: default to the logged-in session token in localStorage so
    // requires_auth endpoints just work for a signed-in player - no per-connection
    // wiring. opts.token still overrides (e.g. a project API key). On a plain
    // game/relay endpoint (no tenant_column) the token only stamps user_id and
    // never changes which room you share, so signed-in and anonymous players still
    // see each other; only a tenant_column endpoint scopes the room to the token's
    // tenant. A missing token is ignored.
    var token = opts.token;
    if (!token) { try { token = localStorage.getItem('auth_token'); } catch (e) {} }
    // Stable identity: a persisted cid so a refresh/reconnect re-attaches to the
    // same player slot instead of becoming a new player (the "opponent
    // disconnected" ghost). The cid is a private reconnect SECRET: the server hashes
    // it into the public id (net.myId / _clientId), so never send it in a frame and
    // never pass a guessable opts.cid (a user id) - whoever knows it takes the slot.
    // Account identity is requires_auth's job.
    // The server accepts 6-64 chars of [A-Za-z0-9-_] and SILENTLY falls back to a
    // fresh per-connection uuid for anything else - which is the exact ghost this
    // parameter exists to prevent - so normalize a short/odd value into that range
    // deterministically instead of letting it be discarded.
    var cid = normalizeClientId(opts.cid) || clientId();
    var reconnect = opts.reconnect !== false;

    function buildURL() {
      var qs = [];
      if (api.room !== '') qs.push('room=' + encodeURIComponent(api.room));
      if (token) qs.push('token=' + encodeURIComponent(token));
      // A room_gated room needs its access token on EVERY connect: the one we were
      // invited with, or the one the server minted for us as the room's first peer.
      var join = opts.join || api.joinToken;
      if (join) qs.push('join=' + encodeURIComponent(join));
      if (opts.meta !== undefined && opts.meta !== null) qs.push('meta=' + encodeURIComponent(JSON.stringify(opts.meta)));
      if (cid) qs.push('cid=' + encodeURIComponent(cid));
      return proto + location.host + '/api/' + path + '/ws' + (qs.length ? '?' + qs.join('&') : '');
    }

    var ws = null;
    var handlers = {};
    var anyHandlers = [];
    var peers = {};
    // connect() returns while the socket is still CONNECTING, so anything sent on
    // the next line would land on a socket that cannot carry it. Queue until open
    // and flush in order: dropping those frames is invisible at the call site and
    // costs whatever the first message set up. The same queue carries frames sent
    // while a dropped socket is re-dialing (bounded, oldest dropped first).
    var pending = [];
    var closedByUser = false;
    var everOpened = false;
    var attempts = 0;
    var retryTimer = null;

    // emit runs the handlers registered for one event name. It is also used for the
    // helper's own synthetic events (__peers, __open, __matched, ...), which is why the
    // onMessage catch-all is NOT driven from here: it is fed the raw server frame, once
    // per frame, in fireAny below.
    function emit(type, msg) {
      var list = handlers[type];
      if (list) { for (var i = 0; i < list.length; i++) { try { list[i](msg); } catch (e) {} } }
    }

    // fireAny hands one parsed server frame to every onMessage subscriber, exactly as
    // received (welcome, join, leave, matched, state, error and app frames alike), after
    // the typed handlers have run. Synthetic events never reach it.
    function fireAny(msg) {
      for (var j = 0; j < anyHandlers.length; j++) { try { anyHandlers[j](msg); } catch (e) {} }
    }

    // emitPeers hands onPeers subscribers the CURRENT full roster. Fired on every
    // change (welcome, join, leave) so a subscriber never reconstructs presence from
    // the individual frames - the mistake that made a joiner see nobody, because it
    // only ever got the join frames for peers who arrived AFTER it.
    function emitPeers() { emit('__peers', api.peers()); }

    // copy returns a shallow copy of a caller's payload, so stamping a routing marker
    // (_action/_discrete/_to) never mutates an object the app keeps using.
    function copy(obj) {
      var o = {};
      if (obj) { for (var k in obj) { if (Object.prototype.hasOwnProperty.call(obj, k)) o[k] = obj[k]; } }
      return o;
    }

    var api = {
      ws: null,
      myId: null,
      role: null,
      index: -1,
      spectator: false,
      players: [],
      state: null,
      rev: -1,
      joinToken: null,
      reconnecting: false,
      room: (opts.room !== undefined && opts.room !== null) ? String(opts.room) : '',
      peers: function () { var out = []; for (var k in peers) { if (peers.hasOwnProperty(k)) out.push({ id: k, meta: peers[k] }); } return out; },
      peerMeta: function (id) { return peers.hasOwnProperty(id) ? peers[id] : null; },
      on: function (type, fn) { (handlers[type] = handlers[type] || []).push(fn); return api; },
      onMessage: function (fn) { anyHandlers.push(fn); return api; },
      onOpen: function (fn) { return api.on('__open', fn); },
      onClose: function (fn) { return api.on('__close', fn); },
      onError: function (fn) { return api.on('__error', fn); },
      onMatched: function (fn) { return api.on('__matched', fn); },
      // onReconnect(cb): a dropped socket came back (same room, same identity). The
      // server replays a fresh welcome, so onPeers/onState re-deliver on their own;
      // use this only for UI ("reconnected"). While re-dialing, net.reconnecting is true.
      onReconnect: function (fn) { return api.on('__reconnect', fn); },
      // onPeers(cb): the room roster changed; cb gets the whole current roster
      // (an array of {id, meta}). Delivered on connect AND on every join/leave, so
      // presence is never rebuilt by hand from welcome/join/leave frames.
      onPeers: function (fn) { return api.on('__peers', fn); },
      // Authoritative endpoints: send an action to the room actor, and render the
      // server's authoritative state pushes. The actor is the source of truth, so
      // do NOT also mutate game state locally from your own action.
      onState: function (fn) { return api.on('__state', fn); },
      action: function (type, payload) { var o = copy(payload); o._action = type; return api.send(o); },
      // send(obj): continuous state - latest-wins, coalesced per tick on a tick
      // endpoint. For position/orientation/cursor, not for messages that must all
      // arrive (use event()).
      send: function (obj) {
        var raw = JSON.stringify(obj || {});
        if (ws && ws.readyState === 1) { ws.send(raw); return api; }
        if (closedByUser) return api;
        pending.push(raw);
        if (pending.length > MAX_QUEUE) pending.shift();
        return api;
      },
      // event(obj): a discrete message that must arrive intact - chat, an emote, a
      // one-off signal. The _discrete marker tells the server to deliver it now
      // rather than fold it into the tick snapshot (where all but the last per tick
      // are dropped), so events and continuous state can share one endpoint.
      // Receivers get it via on(obj.type).
      event: function (obj) { var o = copy(obj); o._discrete = true; return api.send(o); },
      to: function (id, obj) { var o = copy(obj); o._to = id; return api.send(o); },
      switchRoom: function (room, field) { api.room = String(room); var m = {}; m[field || 'room'] = String(room); return api.send(m); },
      close: function () {
        closedByUser = true;
        if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
        try { if (ws) ws.close(); } catch (e) {}
      }
    };

    function scheduleReconnect() {
      api.reconnecting = true;
      // 0.5s, 1s, 2s, 4s, then every 8s: fast enough for a blip, gentle on a server
      // that is down.
      var delay = Math.min(8000, 500 * Math.pow(2, attempts));
      attempts++;
      retryTimer = setTimeout(function () { retryTimer = null; open(); }, delay);
    }

    function open() {
      ws = new WebSocket(buildURL());
      api.ws = ws;
      ws.onopen = function () {
        var wasReconnect = everOpened;
        everOpened = true;
        attempts = 0;
        api.reconnecting = false;
        for (var i = 0; i < pending.length; i++) { ws.send(pending[i]); }
        pending = [];
        emit('__open', {});
        if (wasReconnect) emit('__reconnect', {});
      };
      ws.onclose = function (ev) {
        emit('__close', { code: ev && ev.code, reason: ev && ev.reason });
        // Re-dial only a socket that WAS working and dropped: never after close(),
        // never when a newer connection took our slot, and never a first dial that
        // failed (a refused upgrade - 401, no such endpoint - would just loop).
        if (!reconnect || closedByUser || !everOpened || (ev && ev.code === CLOSE_SUPERSEDED)) return;
        scheduleReconnect();
      };
      ws.onerror = function () { emit('__error', {}); };
      ws.onmessage = function (ev) {
        var m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        dispatch(m);
        fireAny(m);
      };
    }

    // dispatch routes one parsed server frame to the typed handlers and keeps the
    // helper's own state (peers, role, state) current.
    function dispatch(m) {
        if (m._type === 'welcome') {
          api.myId = m._clientId;
          api.spectator = (m._role === 'spectator');
          if (m._joinToken) api.joinToken = m._joinToken;
          peers = {};
          var pl = m._peers || [];
          for (var i = 0; i < pl.length; i++) { peers[pl[i].id] = pl[i].meta || null; }
          emit('welcome', m);
          emitPeers();
          return;
        }
        if (m._type === 'join') { if (m._sender) { peers[m._sender] = (m._meta !== undefined ? m._meta : null); } emit('join', m); emitPeers(); return; }
        if (m._type === 'leave') { if (m._sender) { delete peers[m._sender]; } emit('leave', m); emitPeers(); return; }
        if (m._type === 'matched') {
          api.role = m.role; api.index = m.index; api.players = m.players || [];
          api.switchRoom(m._room, m.room_field); // join the server-assigned room; no client room-id logic
          emit('__matched', m);
          return;
        }
        if (m._type === 'state') { api.state = m.state; api.rev = m.rev; emit('__state', m.state); return; }
        // Surface EVERY server error frame, not just reducer faults. The relay also
        // reports a rejected action, a read-only spectator send, an oversize frame,
        // a throttled frame and an e2ee plaintext rejection this way - all of which
        // would otherwise vanish, because an unhandled frame type is invisible and a stuck
        // client with a clean console reads as a client bug for as long as anyone is
        // willing to look. Listen with net.on('error', fn) to render it in the UI.
        if (m._type === 'error') {
          var detail = '[ofnet] ' + (m._message || (m._fault ? 'reducer fault' : 'server error'));
          if (m._fault) { console.error(detail); } else { console.warn(detail); }
        }
        emit(m.type || m._type, m);
    }

    open();
    return api;
  }
  // roomCode generates a short, human-shareable room code (ambiguous characters
  // removed) for private "play with friends" rooms - use it as the ?room= value:
  //   import { connect, roomCode } from 'ofnet';
  //   const code = roomCode();                     // e.g. "K7P2Q"
  //   const net = connect(path, { room: code });   // share the code; friends
  //   connect(path, { room: theirCode });          // join with it
  // Codes are just room names (the relay auto-creates a room on first join), so no
  // server registration is needed; GET /api/<path>/rooms lists active codes.
  function roomCode(len) {
    var alphabet = 'ABCDEFGHJKMNPQRSTUVWXYZ23456789'; // no I/O/0/1/L
    var n = len || 5;
    var out = '';
    for (var i = 0; i < n; i++) { out += alphabet.charAt(Math.floor(Math.random() * alphabet.length)); }
    return out;
  }

  // normalizeClientId coerces a caller-supplied cid into the form the server
  // accepts (6-64 chars of A-Za-z0-9-_). The mapping is deterministic, so the same
  // input always reclaims the same slot.
  function normalizeClientId(raw) {
    if (raw === undefined || raw === null || raw === '') return null;
    var s = String(raw).replace(/[^A-Za-z0-9\-_]/g, '-');
    if (s.length > 64) s = s.slice(0, 64);
    while (s.length < 6) s = s + '0';
    return s;
  }

  // clientId returns this TAB's stable reconnect secret, persisted in sessionStorage:
  // a refresh or reconnect re-attaches to the same identity (welcome._clientId,
  // derived from it server-side, is then stable) instead of arriving as a brand-new
  // player. Per tab, not per browser: localStorage is shared by every tab, so two tabs
  // (the usual way to try a multiplayer page) would share one identity and keep
  // kicking each other out of the room. It is NOT your public id - that is net.myId.
  function clientId() {
    var k = 'of_cid';
    var store = null;
    try { store = window.sessionStorage; } catch (e) {}
    try {
      var v = store && store.getItem(k);
      if (!v) {
        v = 'c-' + Math.random().toString(36).slice(2, 12) + Math.random().toString(36).slice(2, 12);
        if (store) store.setItem(k, v);
      }
      return v;
    } catch (e) { return null; }
  }

  return { connect: connect, roomCode: roomCode, clientId: clientId };
})();

export const connect = OFNet.connect;
export const roomCode = OFNet.roomCode;
export const clientId = OFNet.clientId;
export default OFNet;
