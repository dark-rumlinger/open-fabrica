---
slug: e2ee
role: capability
title: End-to-end encryption
summary: The E2EE contract (password never reaches the server, fail closed, verify code) plus the platform-owned Web Crypto helper module (import { session } from 'e2ee').
use_when: Messages or calls must be readable ONLY by participants, never the server - a private / secure / end-to-end-encrypted chat or call, or a password-locked room whose operator cannot read the content. Needs a realtime endpoint (relay or signaling).
not_when: Ordinary auth-gated privacy (login plus row scoping) is enough, or the server must read / search / moderate / notify on message content - E2EE makes that impossible by design.
category: security
position: 23
requires: [realtime_signaling]
---

# Optional end-to-end encryption (any private/secure realtime app)

Flag for "secure / private / end-to-end encrypted / password-protected" chat or calls. Crypto is browser-native (Web Crypto); the relay enforces the ciphertext envelope server-side, so a page that skips it cannot send readable messages. Pairs with [[realtime_signaling]] and [[webrtc_signaling]]. Scope: two-party by default (`session`); 3+ via `mode:'group'`.

## Turn it on

`manage_relay action=create endpoint_path=... e2ee=true` (encrypted chat) or `manage_signaling action=create ... e2ee=true` (a WebRTC call whose chat channel is encrypted; media is already peer-to-peer); `action=update` also toggles it on. The flag switches the relay into enforcement mode AND emits `js/e2ee.js` (an ES module, `import { session } from 'e2ee'`) as a platform-managed file - do NOT author, recreate, or delete it. Pages use only its API: never call `crypto.subtle` or improvise key derivation / wire format.

## Modes at a glance

| Mode | Turn on with | Guarantee | Persistence |
|---|---|---|---|
| Two-party (default) | `session({roomId,password})` | ECDH + PBKDF2, fail-closed decrypt | live key only - stored rows are UNREADABLE, add `archive` |
| Group (3+) | `mode:'group'` | membership-change secrecy, pairwise-wrapped epoch key | same - add `archive` |
| Archive (readable history) | `archive:true` | password-derived key, reopens in any later session | `write_to_table` (iv, ct, a) |
| Ratchet | `mode:'group', ratchet:true` | + per-message forward secrecy | NEVER persist (the relay refuses to store ratchet frames) |
| Blobs (files/voice) | `encryptBlob` / `decryptBlob` | chunked AES-GCM under the room key | transient unless you store the ct |
| Identity (always on) | - | trust-on-first-use safety number, key-swap rejection | IndexedDB device key |

**Persisting chat means `archive: true` - there is no other readable lane.** The live message
key comes from an ephemeral per-connection ECDH pair and is never stored, so a row written
under it can never be reopened, not even by its sender. `archive: true` derives the message
key from the room and password alone, so `write_to_table` history reloads on any device from
the same two inputs the user already types. The trade is one-directional: an archived room has
NO forward secrecy, and in group mode `archive:true` with `ratchet:true` throws at construction
(a two-party session has no ratchet lane).

Recall each row's full EXTENDED section (`manage_guides action=recall slug=e2ee section=<anchor>`) before wiring that mode.

## NEVER let the password reach the server

The password IS the key: a server that ever receives it lets anyone with logs/DB access decrypt everything. Hard rules:

- **No POST, no password column.** A room row stores only non-secret fields (name, status, ciphertext).
- **Not in the query string** (`?pw=`) - it lands in access logs. The WS URL may carry `?room=<name>`, never the password.
- **Client-only.** Carry it page-to-page in the URL **fragment** (`'#pw=' + encodeURIComponent(pw)`, never sent to the server; read `location.hash` then `history.replaceState` to wipe it) or retype it; it goes into `session({ roomId, password })` and nowhere else.
- **Not even a HASH** - a fast hash of a weak password is brute-forceable offline. For a join gate, mint a SEPARATE high-entropy token (`crypto.randomUUID()`) and store/check THAT server-side, distinct from the password.

## The helper API

```js
import { connect } from 'ofnet';
import { session } from 'e2ee';
const net = connect('<endpoint>', { room: roomId });  // always connect(), never a raw socket
const e2ee = session({ roomId, password });  // password shared out-of-band
net.onOpen(() => e2ee.start(net.ws));  // generate + announce ECDH public key
e2ee.onReady(() => enableComposer());  // key derived - only now allow sending
net.onMessage(async (m) => {
  if (await e2ee.handleSignal(m, net.ws)) return;  // consumes 'e2ee-pub' handshake frames
  if (m.type === 'chat' && m.ct) {
    const text = await e2ee.decrypt(m);  // null on failure/replay - drop, do not render
    if (text != null) renderMessage(text, m._sender);
    return;
  }
  // welcome / join / leave (and offer/answer/ice for calls) are still yours
});
// sending (composer disabled until onReady):
net.event(await e2ee.encrypt(text, net.myId));  // net.myId = welcome._clientId; event = the discrete lane
// verify (defeats a relay MITM): compare aloud; for calls feed SDP first
e2ee.setFingerprints(pc.localDescription.sdp, pc.remoteDescription.sdp);
showVerifyCode(await e2ee.verifyCode());
```

## What the relay enforces (e2ee=true endpoints)

{{contract:e2ee-wire}}

Give the persistence table `iv`, `ct` and `a` columns; your own `verify-confirm`/`typing`/presence frames also pass in the clear. Only put secret words through `encrypt()`.

<!-- extended -->

## Calls: entry gating
> when: video/voice calls - gate entry with the per-room token; call media is NOT bound to the password.

The `e2ee` helper encrypts the chat data channel only; call **media is peer-to-peer DTLS-SRTP and NOT bound to the password** - any peer who reaches the room sees/hears the other side:

- **Gate ENTRY with the per-room token.** `room_gated=true` (DEFAULT on a `manage_signaling` endpoint with `e2ee`) makes the relay 401 the WS upgrade without a matching `?join=<token>`. The FIRST peer claims the room and gets `welcome._joinToken`; put it in the invite link (`/room/<name>?join=<token>#pw=<password>`). Don't store or POST the token.
- **Hold/blur remote media until the user confirms the verify code matches aloud.** A token-holding peer with the WRONG password produces a DIFFERENT `verifyCode()`. Never auto-reveal.

The invite link is the ONLY way a second peer enters a `room_gated` room (a tokenless connect 401s) - render it into the waiting UI:

```js
// Claiming peer: build the link AND write it into the waiting UI (a never-wired <input> loads blank, no error):
if (msg._joinToken) dom.inviteLink.value = location.origin + '/room/' + encodeURIComponent(roomName) +
    '?join=' + encodeURIComponent(msg._joinToken);  // password rides the #fragment
// Entering peer: read ?join= and pass it to connect(), which puts it on the WS URL:
const net = connect('<endpoint>', { room: roomName, join: new URLSearchParams(location.search).get('join') });
```

Do NOT render UI implying name + password alone lets the second peer in.

## Group rooms
> when: 3+ participants (mode:'group') with a pairwise-wrapped epoch key.

`mode:'group'` and feed the helper the roster. The crypto is a **pairwise-wrapped epoch key**: one AES-GCM key per "epoch", re-minted on every membership change and delivered wrapped under a key only each member's password opens. Messages ride the same enforced envelope plus an epoch `ep` tag; the only extra frame is the bodyless `e2ee-gkey`. Wiring = the core shape plus:

```js
const g = session({ roomId, password, mode: 'group' });
g.onReady(() => enableComposer());  // first epoch key installed - only now allow sending
net.onPeers((peers) => {  // the WHOLE roster, on connect AND every join/leave
  g.setSelf(net.myId);  // call before onRoster; onRoster re-adds self for you
  g.onRoster(peers.map((p) => p.id));
});
// handleSignal also consumes e2ee-gkey; decrypt() is null for an epoch you don't hold - drop, don't render.
net.event(await g.encrypt(text));  // tagged with the current epoch (self already set via setSelf, so no id arg here, unlike two-party encrypt(text, net.myId))
showVerifyCode(await g.verifyCode());  // covers every member's public key
```

Membership secrecy lives at the join/leave boundary (a JOIN locks out earlier messages, a LEAVE later ones). `onRoster` needs the FULL member list every time - feed it from `net.onPeers`; hand-rebuilding from `join`/`leave` frames risks a missed `leave` and an un-rotated key. No "host": the helper elects the distributor (lowest client id) and hands over automatically.

**`onReady` needs a SECOND member - never show "deriving…" forever.** The key cannot exist until two members are present; show "waiting for someone to join" while `onRoster` reports one member, composer still gated on `onReady`.

## Per-message forward secrecy (ratchet)
> when: ephemeral rooms needing per-message forward secrecy (ratchet:true).

Add `ratchet: true` (group mode): each message gets its own key, deleted after decrypt, so a later device compromise cannot recover already-read messages; both ends derive the same per-sender chain from the epoch key - no extra setup:

```js
const g = session({ roomId, password, mode: 'group', ratchet: true });
// everything else (start / setSelf / onRoster / encrypt / decrypt) is identical.
```

Because used keys are destroyed, a reload CANNOT redisplay earlier messages - **ephemeral rooms only; never pair `ratchet:true` with a persisted `write_to_table`** (the relay refuses to persist a ratchet frame and answers the sender with a one-time error frame, so history comes back empty rather than unreadable; subscribe to `net.on('error', ...)` to see it). A 1:1 ratcheted chat = group mode with two people.

## Safety numbers
> when: durable cross-session contact verification (persistent device identity, trust on first use).

Every device has a long-lived ECDSA identity key (IndexedDB, never sent in the clear) that signs each session's ephemeral key, so a relay that swaps in its own key is rejected. `verifyCode()` changes per session and also covers a call's DTLS fingerprint (the LIVE check on a call); `safetyNumber()` is stable - pin/verify it ONCE per durable contact (trust on first use) and you are warned if the key later changes. On a call, do both.

```js
showSafetyNumber(await sess.safetyNumber());  // stable digits - compare out-of-band ONCE
const st = await sess.verifyStatus('alice');  // 'alice' = a durable contact label YOU pick
if (!st.present)      showPending();          // no peer key yet - check st.present FIRST
else if (st.idError)  showError('Could not verify the other side - possible tampering. Do not proceed.');
else if (st.firstUse) { await sess.pin('alice'); showNewContact(); }  // remember the key
else if (st.changed)  showWarning('Safety number CHANGED - re-verify before trusting.');
else if (st.verified) showVerifiedBadge();
onUserConfirmsMatch(() => sess.markVerified('alice'));  // user compared aloud - it matches
```

**`st.present` is false until the handshake completes**, and every other flag reads as first-use until then - a status rendered on page load claims "new contact" for a peer you have never seen, and its `pin()` is a silent no-op.

Group: `safetyNumber()` covers every member; `verifyStatus` / `pin` / `markVerified` take `(clientId, contactId)` - the member's client id plus your contact label. A group session also needs **`setSelf(net.myId)` before its first `encrypt()`**: your own client id is bound into the ciphertext's authenticated data, so encrypting before it is set produces a frame nobody can open. `identity()` (from `'e2ee'`) resolves the device's own key (`.pubB64`). Identity is per-DEVICE: a new browser/profile or cleared storage is a NEW identity, so a changed safety number means "re-verify," not necessarily an attack.

## Encrypted files, images and voice
> when: sending encrypted files, images, or voice messages (blobs).

`encryptBlob` / `decryptBlob` chunk and AES-GCM-encrypt a file under the room key; the relay sees only ciphertext. `encryptBlob` returns `{ meta:{v,name,type,size,n}, chunks:[...] }`. Over the RELAY you MUST send each chunk as its OWN frame - one frame for the whole file exceeds `max_msg_size` (default 16384 bytes) and is dropped with an in-band `message dropped: size N exceeds limit` error frame; a frame far over the limit closes the socket (1009). Size `chunkSize` UNDER `max_msg_size` (base64 adds ~37%, so `12000` -> ~16 KB, which leaves only a few hundred bytes of margin at the default - raise `max_msg_size` on the endpoint or use a smaller `chunkSize`).

```js
// SEND over the relay (file input, drag-drop, or MediaRecorder Blob):
const blob = await sess.encryptBlob(await file.arrayBuffer(), { name: file.name, type: file.type, chunkSize: 12000 });
const id = crypto.randomUUID();
net.event({ type: 'file-meta', id, meta: blob.meta });          // net = connect(...) from 'ofnet'
blob.chunks.forEach((data, i) => net.event({ type: 'file-chunk', id, i, data }));
// (WebRTC DataChannel: no relay cap - send the whole blob at once.)

// RECEIVE: collect by id, decrypt once all meta.n arrived.
const inbox = {};  // id -> { meta, chunks:[] }
function onFileMeta(m)  { inbox[m.id] = { meta: m.meta, chunks: new Array(m.meta.n) }; }
async function onFileChunk(m) {
  const f = inbox[m.id]; if (!f) return;
  f.chunks[m.i] = m.data;
  if (f.chunks.filter(Boolean).length !== f.meta.n) return;
  const bytes = await sess.decryptBlob(f);  // Uint8Array, or null if any chunk failed/was tampered
  delete inbox[m.id];
  if (bytes) img.src = URL.createObjectURL(new Blob([bytes], { type: f.meta.type }));  // or download link / <audio>
}
```

- A voice/video message is just a `MediaRecorder` Blob: `encryptBlob(await recorded.arrayBuffer(), {type:'audio/webm'})`.
- `file-meta`/`file-chunk` carry no plaintext body: the relay forwards but does NOT persist them - a file is transient unless you store the ciphertext.
- Group rooms: same, keyed to the current epoch (NOT per-message ratcheted, even with `ratchet:true`).

## Readable history (archive)
> when: the room persists chat with write_to_table and history must reload on a later visit.

`archive: true` on `session(...)` / `groupSession(...)` swaps the message key for one derived
from `PBKDF2(password, salt=roomId)` alone. Nothing else in the wiring changes: `encrypt()`
emits the same envelope plus `a:1`, and `decrypt()` reads a live frame and a replayed history row
through the same call, so the history loader is just a `create_api` read piped into the handler
you already wrote.

```js
const sess = session({ roomId, password, archive: true });   // + mode:'group' for 3+
// history: rows come back as {iv, ct, a, created_at, ...} from the persistence table
for (const row of (await (await fetch('/api/messages?room=' + roomId)).json()).rows) {
  const text = await sess.decrypt(row);       // same call as a live frame
  if (text != null) renderMessage(text, row.created_at);
}
```

The handshake still runs and still gates the composer on `onReady`: that is what authenticates
the peer and backs `verifyCode()`. What changes is only which key the message body rides.
`encryptArchive()` / `decryptArchive()` are the explicit entry points if a room wants to archive
only some messages and keep the rest on the forward-secret live key.

Two hard rules: in group mode `archive:true` + `ratchet:true` throws (the ratchet destroys the keys
an archive exists to reopen), and an archived room's copy says "encrypted in your browser, history readable
with the room password" - never an unqualified forward-secrecy claim.

## Automatic cleanup
> when: persistence and disappearing-message (TTL) rules.

`ephemeral:true` is the DEFAULT once `e2ee:true`: a burner room whose persisted rows the relay deletes when it empties - no client cleanup (the `beforeunload`/`sendBeacon` ban is in the [[realtime_signaling]] guide); on hang-up send a `bye` and close the socket. That is the right default for a throwaway call or chat, where history is not meant to survive.

**Durable scroll-back history is the opposite setting: the client's `archive:true` AND the relay's `keep_history=true`** (on `manage_relay` create or update; it turns the e2ee ephemeral default off). Archive persists chat through `write_to_table` (`iv`/`ct`/`a` columns) under the password-derived key so a later visitor reloads it (Readable history above), but the empty-room sweep would delete exactly those rows - so a room that keeps history MUST turn the ephemeral default off, or the archive is gone the moment everyone disconnects. For **disappearing messages** on a kept-history room, set `message_ttl_seconds=<seconds>` instead - the relay sweeps rows older than the TTL; pair with a client-side hide-by-age so they also vanish from an already-open window.

## Keep the UI honest
> when: writing the UI - fail-closed composer/media and honest verify copy.

- Copy: "Encrypted in your browser - the server stores only ciphertext" + a verify affordance (live `verifyCode()` on a call, persistent `safetyNumber()` for a contact). Do NOT show an unqualified "E2EE" badge.
- **Fail closed.** Disable the composer and hold media until `onReady` fires. Never write `if (password && session) {...} else { ready = true }` - that ships a silently insecure call (the relay drops plaintext chat but media still flows, with no verify banner). A missing key means STOP and show the error state, not "enable it anyway".
- Call teardown (a `leave`/`bye` before `iceConnectionState` is `connected` is NOT the call ending - tear down the half-open `RTCPeerConnection` and return to waiting) and Perfect Negotiation are the [[webrtc_signaling]] contract; follow it verbatim.

## Honest limits
> when: telling the owner what the guarantees are and are not.

**`archive:true` trades forward secrecy for readable history** (mechanism under Readable history):
the password-derived key does not rotate, so anyone who ever learns the password can read every
stored row, past and future. That is the correct trade for a chat the owner expects to scroll
back through, and the wrong one for a burner room - there, leave archive off and reach for
`ratchet:true` if the room also needs per-message forward secrecy.

Group rooms give **membership-change** secrecy, not per-sender cryptographic identity: every present member shares the epoch key (`_sender` is relay-stamped and unforgeable, but anyone present can read or re-share the key), so persisted ciphertext is only as private as membership and password discipline - prefer a generated high-entropy room code. `ratchet:true` adds forward secrecy for consumed messages, but the distributor keeps the epoch root while the room lives - not full post-compromise security. Media never touches the server (p2p DTLS-SRTP); the verify code closes the residual relay-MITM gap; the identity key stops an in-transit key swap but does not bind a real human to a key.

## How the helper works
> when: a deep reference on the crypto internals (platform-owned - you do not author it).

All crypto lives in the platform-managed `js/e2ee.js` (`import { session, groupSession, identity } from 'e2ee'`); the relay sees only the opaque envelope the `e2ee-wire` contract fixes. Two-party `session`: ECDH P-256 + PBKDF2(password, salt=roomId) -> HKDF -> non-extractable AES-GCM-256, fresh IV per message, AEAD sender/room binding, replay counter, fail-closed decrypt. Group (`groupSession(...)` also works): the epoch-key scheme under **Group rooms**; identity: an ECDSA P-256 device key signs the session key; ratchet: a per-sender hash chain deleting used keys; blobs: chunked AES-GCM.
