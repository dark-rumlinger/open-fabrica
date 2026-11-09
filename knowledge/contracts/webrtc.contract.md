---
id: webrtc
title: WebRTC signaling wire contract
kind: wire-envelope
governs: [webrtc_signaling]
probes: [Perfect Negotiation, manage_signaling]
---
**WebRTC signaling rides the realtime relay** - a signaling endpoint, created with the `manage_signaling` tool (which raises `max_msg_size` 16KB->64KB so SDP offers aren't dropped, and disables tick coalescing so signaling is immediate), connected room-scoped with `?room=<id>` (with `e2ee=true` the endpoint is room-gated: the first peer gets a `_joinToken` in its `welcome`, and every later peer must connect with `&join=<that token>`). Peers exchange these app frames over the relay (each broadcast to the room with `_sender` injected); use raw `RTCPeerConnection`, never a wrapper lib (simple-peer/peerjs):

```js
{ type: 'hello',  id: myClientId }   // announce self after welcome
{ type: 'offer',  sdp }              // SDP offer
{ type: 'answer', sdp }              // SDP answer
{ type: 'ice',    candidate }        // a single ICE candidate (may arrive BEFORE the SDP)
{ type: 'bye' }                      // hang up
```

**Role is W3C Perfect Negotiation, never a `_clients` count.** After `welcome` save `myClientId = _clientId`; on the peer's `hello`/`join` store `peerId` and derive `polite = myClientId > peerId` (any deterministic tiebreaker). Let `onnegotiationneeded` drive the offer; on a colliding offer the impolite side ignores it and the polite side rolls back. Buffer ICE candidates until `setRemoteDescription` resolves, then flush. Include a TURN server (not STUN alone) in `iceServers`.
