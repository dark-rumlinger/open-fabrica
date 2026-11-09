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

# WebRTC signaling - the only correct pattern (peer-to-peer audio/video calls)

This rides the realtime relay (see [[realtime_signaling]] for relay frames and presence seeding) - the additional contract for projects placing `getUserMedia` audio/video calls between peers. A text-only app needs none of it.

{{contract:webrtc}}

The sections below are the full client implementation of that contract.

Raw `RTCPeerConnection` only: a wrapper library (`simple-peer`, `peerjs`) has its own
signalling API that silently breaks the exchange below. WebRTC needs five things right:
glare-safe role selection, ICE buffering, a connection-state status indicator, a TURN-backed
NAT config, and the right WS plumbing.

**1. Role (the contract's Perfect Negotiation).** A `_clients` count races on a fast reload
and has no glare handling. After `welcome` send a one-shot `{type:'hello', id: myClientId}`;
the peer id comes from its `hello` or `join` (`msg.id || msg._sender`). Create the
`RTCPeerConnection` as soon as `getUserMedia` resolves, `addTrack` every local track up front,
and never call `createOffer` from the welcome/join handler. Set `polite = myClientId > peerId`
once the peer id is known; left at the canonical client's `false`, both peers are impolite
and a glare collision is ignored on both sides.

**2. ICE can arrive before the SDP - buffer until `setRemoteDescription` resolves, then flush; collisions roll back on the polite side.** Both sides need this: `onicecandidate` fires right after `setLocalDescription`, often before the peer processed the offer. Canonical client:

```js
let pc = null, polite = false, makingOffer = false, ignoreOffer = false; // polite = myClientId > peerId, once known
let iceQueue = [], remoteSet = false;

function createPeerConnection() {
  pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });
  localStream.getTracks().forEach(t => pc.addTrack(t, localStream));

  pc.onnegotiationneeded = async () => {
    try {
      makingOffer = true;
      await pc.setLocalDescription();           // implicit createOffer
      send({ type: 'offer', sdp: pc.localDescription.sdp });
    } catch (e) { console.error(e); }
    finally { makingOffer = false; }
  };

  pc.onicecandidate = ({ candidate }) => {
    if (candidate) send({ type: 'ice', candidate });
  };

  pc.ontrack = e => { remoteVideo.srcObject = e.streams[0]; };
  pc.oniceconnectionstatechange = updateStatus;   // see section 3
  pc.onconnectionstatechange    = updateStatus;
}

function flushIce() {
  while (iceQueue.length) pc.addIceCandidate(iceQueue.shift()).catch(()=>{});
}

async function handleOffer(msg) {
  const collision = makingOffer || pc.signalingState !== 'stable';
  ignoreOffer = !polite && collision;
  if (ignoreOffer) return;
  if (collision) await pc.setLocalDescription({ type: 'rollback' });
  await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
  remoteSet = true; flushIce();
  await pc.setLocalDescription();               // implicit createAnswer
  send({ type: 'answer', sdp: pc.localDescription.sdp });
}

async function handleAnswer(msg) {
  await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
  remoteSet = true; flushIce();
}

function handleIce(cand) {
  if (pc && remoteSet) pc.addIceCandidate(cand).catch(()=>{ if (!ignoreOffer) throw 0; });
  else iceQueue.push(cand);
}
```

Do NOT flush the queue inside `createPeerConnection` - `pc.remoteDescription` is still null there and every `addIceCandidate` rejects with `InvalidStateError`; catching that silently drops candidates and the call hangs forever. **Tracks MUST be added before the first `onnegotiationneeded`** (inside `createPeerConnection`, not lazily inside `ontrack`) - otherwise the remote side has no media and `ontrack` never fires on either peer.

**3. Status indicator MUST be driven from `pc.iceConnectionState`/`pc.connectionState`, never set once in the welcome handler.** The common bug: the welcome handler sets `status = 'Waiting for peer…'` and nothing overwrites it as ICE progresses - chat works, SDP/ICE exchange, but the user stares at "Waiting for peer…" forever. Implement one `updateStatus()` that reads `pc.iceConnectionState` and writes the indicator, wired to BOTH `oniceconnectionstatechange` AND `onconnectionstatechange` so it can't get stuck. Mandatory mapping:

| `pc.iceConnectionState` (or no peer yet) | UI label              |
|--------------------------------------|-----------------------|
| no `hello` / `join` received yet            | "Waiting for peer…"   |
| `new` / `checking`                       | "Connecting…"         |
| `connected` / `completed`                | "Connected"           |
| `disconnected`                          | "Reconnecting…"       |
| `failed` / `closed`                      | "Connection failed"   |

**4. Include a TURN server in `iceServers` - STUN alone is not enough.** Symmetric NAT/CGNAT users cannot hole-punch; public STUN (`stun:stun.cloudflare.com:3478`) covers ~70% of pairs and the rest need a relay. Free shared TURN (`openrelay.metered.ca:80`) is testing-only and often blocked, so as the only relay it reproduces the section-3 silent fail. For production use a `turns:` (TLS on 443) relay from a real provider (Cloudflare Calls, Twilio, coturn): keep its key in the secret store and mint short-lived `iceServers` credentials in a `manage_function` the client fetches before connecting. Never hardcode a shared TURN credential in client JS; with none configured, fall back to STUN-only and say in the UI it "may not work across some networks".

**5. Connect room-scoped, through the `ofnet` helper.** Sessions MUST be room-scoped on the WS so signaling for room A doesn't leak into room B: `import { connect } from 'ofnet'; connect('signaling', { room: roomId })`. A raw socket also skips the `?join=` handling `room_gated` calls need.

**6. Create the endpoint with `manage_signaling`.** A 1:1 call is `manage_signaling create endpoint_path=call max_clients=2 room_gated=true` (invite-only: knowing the room name must not be enough to pull the media); add `e2ee=true` for a private call's chat. The tool itself raises `max_msg_size` and disables tick coalescing so SDP offers stay whole and immediate (contract above) - there is no mode to set.

## A peer leave is NOT the call ending (a common "call ends the instant I join" mistake)

A reload or stale roster entry fires a `leave` for a peer you never finished connecting to. Treating every `leave`/`bye` as a terminal "call ended" kills the call on join. Rules:

- **Track whether you ever connected** - set `connected = true` only when `pc.iceConnectionState` reaches `connected`/`completed`.
- **On `leave`/`bye` for the current peer BEFORE `connected`:** clear `peerId`, `pc.close()`, reset negotiation flags, return to waiting. Do NOT show an ended screen - the real peer can still join.
- **Only after `connected`** does a `leave`/`bye` mean the call genuinely ended; show the ended screen then.
- **Ignore stale self:** a `leave` whose `_sender` isn't your current `peerId` is noise; drop it.

<!-- extended -->

## Chat works but video doesn't
> when: the relay is clearly fine (chat arrives) and the call still will not connect.

A working chat proves the WS relay, room scoping and `_sender` injection are fine, so the bug is in the WebRTC client. Three checks, in order:

1. **Is the status indicator wired to `pc.iceConnectionState`?** (core, section 3) Set once on `welcome` and never reassigned, the UI lies and the call may in fact be connected. Rule this out first or you debug a working call.
2. **Is TURN reachable?** In `chrome://webrtc-internals` mid-call: ICE candidates of type `host`/`srflx` but no `relay` means TURN is dead - wrong credential, firewalled port, or a rate-limited public server.
3. **Were tracks added before the first offer?** (core, section 2) `pc.addTrack` must run inside `createPeerConnection`, not lazily inside `ontrack`, or `ontrack` never fires on the remote side.

## Screen sharing
> when: adding or swapping a screen-share / display track on the call.

Screen share is just another media track: `getDisplayMedia` gives you a stream and you put its video
track on the SAME `RTCPeerConnection`. It is end-to-end protected exactly like the camera (peer-to-peer
DTLS-SRTP) - the verify code already covers it, nothing e2ee-specific to add. Two patterns:

**Swap the outgoing video (recommended - no renegotiation, no glare).** Use `replaceTrack` on the
existing video sender to switch camera <-> screen. The m-line count does not change, so this does NOT
fire `onnegotiationneeded` and sidesteps offer/answer glare entirely:

```js
let cameraTrack = localStream.getVideoTracks()[0];

async function startScreenShare() {                    // MUST be called from a click handler
  const display = await navigator.mediaDevices.getDisplayMedia({ video: true });
  const screenTrack = display.getVideoTracks()[0];
  const sender = pc.getSenders().find(s => s.track && s.track.kind === 'video');
  await sender.replaceTrack(screenTrack);              // swap, no renegotiation
  screenTrack.onended = () => stopScreenShare();       // user clicked the browser "Stop sharing" bar
}

async function stopScreenShare() {
  const sender = pc.getSenders().find(s => s.track && s.track.kind === 'video');
  await sender.replaceTrack(cameraTrack);              // back to camera
}
```

**Send screen AS WELL AS camera (a second track).** Call `pc.addTrack(screenTrack, display)` instead -
this DOES change the m-lines and fires `onnegotiationneeded`, which the section-2 Perfect Negotiation
flow renegotiates for you (never call `createOffer` by hand). The receiver gets a second `ontrack`; key
your `<video>` elements by stream/`track.id` so the screen tile does not overwrite the camera tile.

Gotchas: `getDisplayMedia` must run inside a user gesture or the browser blocks it; always wire
`screenTrack.onended` so the browser's own "Stop sharing" control returns to camera; audio share
(`getDisplayMedia({audio:true})`) is a separate track - swap/add it on the audio sender the same way.

## Room cap and cleanup
> when: capping room size, or tearing down the connection on hang-up.

Cap the room on the `manage_signaling` endpoint itself with `max_clients=N` (`2` for a 1:1 call, `3+` for a small group): the relay enforces it atomically on join and admits the overflow peer as a read-only spectator (`net.spectator === true`) - see the [[realtime_signaling]] guide's room-capacity section for the mechanism. Don't try to hold a peer cap in client JS (two simultaneous joins slip past it). A signaling endpoint persists nothing, so there is no room state to clean up (the `ephemeral` + `write_to_table` pruning in the [[realtime_signaling]] contract applies to a relay that writes rows, not here); on hang-up just send a `bye`, `pc.close()`, and close the socket - never a client `beforeunload` DELETE.
