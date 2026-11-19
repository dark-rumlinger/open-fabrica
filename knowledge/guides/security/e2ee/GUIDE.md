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

# Optional end-to-end encryption

Flag for private/E2EE chat or calls. Pairs with [[realtime_signaling]] and [[webrtc_signaling]].

## Turn it on

`manage_relay action=create ... e2ee=true` or `manage_signaling action=create ... e2ee=true` (`action=update` toggles it too). This enables relay ciphertext enforcement and emits platform-managed `js/e2ee.js` (`import { session, groupSession, identity } from 'e2ee'`) — never author/edit/delete it or call `crypto.subtle` directly.

## Modes at a glance

| Mode | Turn on with | Guarantee | Persistence |
|---|---|---|---|
| Two-party (default) | `session({roomId,password})` | ECDH + PBKDF2, fail-closed decrypt | Live key only (stored rows unreadable; add `archive`) |
| Group (3+) | `mode:'group'` | Membership-change secrecy, pairwise-wrapped epoch key | Same (add `archive`) |
| Archive (readable history) | `archive:true` | Password-derived key, reopens later (no forward secrecy) | `write_to_table` (`iv`, `ct`, `a`) |
| Ratchet | `mode:'group', ratchet:true` | + per-message forward secrecy | Never persist (relay refuses; incompatible with `archive:true`) |
| Blobs (files/voice) | `encryptBlob` / `decryptBlob` | Chunked AES-GCM | Transient unless stored |
| Identity (always on) | - | TOFU safety number, key-swap rejection | IndexedDB device key |

Recall the matching extended section (`manage_guides action=recall slug=e2ee section=<anchor>`) before wiring that mode.

## NEVER let the password reach the server

- Never POST the password or store it in a table (not even hashed — use a separate `crypto.randomUUID()` join token for server-side gates).
- Never pass the password in `?query` strings; pass page-to-page only via URL `#fragment` (`'#pw=' + encodeURIComponent(pw)`, wiped with `history.replaceState`) or re-entry.

## The helper API

```js
import { connect } from 'ofnet';
import { session } from 'e2ee';
const net = connect('<endpoint>', { room: roomId });
const e2ee = session({ roomId, password });
net.onOpen(() => e2ee.start(net.ws));
e2ee.onReady(() => enableComposer()); // fail closed until ready
net.onMessage(async (m) => {
  if (await e2ee.handleSignal(m, net.ws)) return;
  if (m.type === 'chat' && m.ct) {
    const text = await e2ee.decrypt(m);
    if (text != null) renderMessage(text, m._sender);
  }
});
net.event(await e2ee.encrypt(text, net.myId));
// verify (for calls, feed SDP first):
e2ee.setFingerprints(pc.localDescription.sdp, pc.remoteDescription.sdp);
showVerifyCode(await e2ee.verifyCode());
```

## What the relay enforces (e2ee=true endpoints)

{{contract:e2ee-wire}}

Give any persistence table `iv`, `ct`, and `a` columns; `verify-confirm`/`typing`/presence frames pass in the clear.

<!-- extended -->

## Calls: entry gating
> when: video/voice calls - gate entry with the per-room token; call media is NOT bound to the password.

Call media is P2P DTLS-SRTP (not password-bound). `room_gated=true` (default on `manage_signaling` with `e2ee=true`) requires `?join=<token>` (`welcome._joinToken` for the first peer) — render `/room/<name>?join=<token>#pw=<password>` in the waiting UI and pass `join` to `connect()`. Hold/blur remote media until the user confirms `verifyCode()` matches aloud.

## Group rooms & ratchet
> when: 3+ participants (`mode:'group'`) or per-message forward secrecy (`ratchet:true`).

- **Group (`mode: 'group'`):** call `g.setSelf(net.myId)` before `g.onRoster(peers.map(p => p.id))` inside `net.onPeers`, and send via `net.event(await g.encrypt(text))` (no `id` arg). `onReady` waits for a second member — show "waiting for someone to join" when solo.
- **Ratchet (`ratchet: true`):** ephemeral rooms only; never pair with `write_to_table` or `archive: true` (throws). Subscribe to `net.on('error', ...)`.

## Safety numbers
> when: durable cross-session contact verification (TOFU).

Check `const st = await sess.verifyStatus('alice')` (`(clientId, 'alice')` in group mode): wait for `st.present` before acting; handle `st.idError`, `st.firstUse` (`await sess.pin('alice')`), `st.changed`, `st.verified`, and `sess.markVerified('alice')`. Identity keys are per-device (IndexedDB).

## Encrypted files, images and voice
> when: sending encrypted files, images, or voice messages (blobs).

Over a relay, `encryptBlob(buf, { name, type, chunkSize: 12000 })` (under `max_msg_size` 16384B) must be sent chunk-by-chunk (`file-meta` + `file-chunk`), reassembled in order, and decrypted with `decryptBlob()`. Over a WebRTC DataChannel, send the whole blob at once.

## Readable history (archive)
> when: persisting chat history with `write_to_table` or disappearing messages.

- `e2ee=true` defaults to `ephemeral: true` (rows drop when the room empties).
- For durable history, set **both** `archive: true` on `session(...)` **and** `keep_history=true` on `manage_relay`. For disappearing messages, add `message_ttl_seconds=<sec>` + client-side age hiding.

## Keep the UI honest
> when: writing the UI - fail-closed composer/media and honest verify copy.

- Disable composer and hold media until `onReady`; never fall back to unencrypted sending.
- Show "Encrypted in your browser - the server stores only ciphertext" (for `archive: true`, note history is readable with the room password) plus a verify affordance; never show an unqualified "E2EE" badge.
- Follow [[webrtc_signaling]] for Perfect Negotiation and early `leave`/`bye` teardown.
