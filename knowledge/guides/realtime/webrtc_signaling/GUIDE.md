---
slug: webrtc_signaling
role: capability
title: WebRTC calls
summary: Peer-to-peer audio/video/screen-share calls between users (WebRTC) - offer/answer negotiation, ICE buffering, TURN relay, call teardown.
use_when: Users hold peer-to-peer audio, video, or screen-share calls with each other.
not_when: Text-only chat, one-way broadcast, or any project with no live call between users.
evidence_gated: true
category: realtime
position: 22
tools: [manage_signaling]
requires: [realtime_signaling]
decisions: [realtime model/webrtc, realtime model/signaling]
---

# WebRTC signaling (peer-to-peer audio/video calls)

Rides [[realtime_signaling]]. Raw `RTCPeerConnection` only (never `simple-peer` or `peerjs`).

{{contract:webrtc}}

## Platform wiring & Perfect Negotiation rules

1. **Create endpoint:** `manage_signaling create endpoint_path=call max_clients=2 room_gated=true` (add `e2ee=true` for encrypted chat). Connect room-scoped via `'ofnet'`: `connect('signaling', { room: roomId })` (raw WebSocket skips `?join=`).
2. **Role & tracks:** after `welcome`, send `{ type: 'hello', id: myClientId }`; set `polite = myClientId > peerId` once the peer's id arrives (`msg.id || msg._sender`). Create `RTCPeerConnection` and call `addTrack` for all local tracks *before* `onnegotiationneeded` fires.
3. **Buffer ICE:** queue incoming `ice` candidates until `setRemoteDescription` resolves, then flush; roll back on offer collision if `polite`.
4. **Status indicator:** drive UI status from a single `updateStatus()` bound to both `oniceconnectionstatechange` and `onconnectionstatechange`:
   - no peer yet → `"Waiting for peer…"` | `new`/`checking` → `"Connecting…"` | `connected`/`completed` → `"Connected"` | `disconnected` → `"Reconnecting…"` | `failed`/`closed` → `"Connection failed"`.
5. **TURN server:** public STUN (`stun:stun.cloudflare.com:3478`) fails behind symmetric NAT/CGNAT, and `openrelay.metered.ca` is testing-only. For production TURN (`turns:` on 443), store provider keys in secrets and mint short-lived `iceServers` in a `manage_function`; never hardcode TURN credentials in client JS (if unconfigured, fall back to STUN-only and note it in the UI).
6. **Leave vs call end:** track `connected = true` only once `iceConnectionState` hits `connected`/`completed`. Drop `leave`/`bye` frames whose `_sender !== peerId`. If `leave`/`bye` arrives *before* `connected`, close/reset `pc` and return to waiting — show the ended screen only if `connected` was true.

<!-- extended -->

## Chat works but video doesn't
> when: the relay is clearly fine (chat arrives) and the call still will not connect.

Check in order: (1) `updateStatus()` is wired to `iceConnectionState` (not stuck on `welcome`), (2) `chrome://webrtc-internals` shows `relay` ICE candidates (TURN reachable), and (3) `pc.addTrack` ran before the first offer.

## Screen sharing
> when: adding or swapping a screen-share / display track on the call.

Prefer `sender.replaceTrack(screenTrack)` on the existing video sender (no renegotiation/glare; restore `cameraTrack` on `screenTrack.onended`). To send screen *alongside* camera, `pc.addTrack(screenTrack, display)` (renegotiates via `onnegotiationneeded`) and key `<video>` elements by stream/`track.id`.

Gotchas: `getDisplayMedia` must run inside a user gesture or the browser blocks it; always wire
`screenTrack.onended` so the browser's own "Stop sharing" control returns to camera; audio share
(`getDisplayMedia({audio:true})`) is a separate track - swap/add it on the audio sender the same way.

## Room cap and cleanup
> when: capping room size, or tearing down the connection on hang-up.

Cap the room on the `manage_signaling` endpoint itself with `max_clients=N` (`2` for a 1:1 call, `3+` for a small group): the relay enforces it atomically on join and admits the overflow peer as a read-only spectator (`net.spectator === true`) - see the [[realtime_signaling]] guide's room-capacity section for the mechanism. Don't try to hold a peer cap in client JS (two simultaneous joins slip past it). A signaling endpoint persists nothing, so there is no room state to clean up (the `ephemeral` + `write_to_table` pruning in the [[realtime_signaling]] contract applies to a relay that writes rows, not here); on hang-up just send a `bye`, `pc.close()`, and close the socket - never a client `beforeunload` DELETE.
