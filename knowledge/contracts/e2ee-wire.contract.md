---
id: e2ee-wire
title: E2EE chat wire envelope
kind: wire-envelope
governs: [e2ee]
probes: [type:'chat']
---
**E2EE wire envelope (`e2ee=true` endpoints).** A human-readable message MUST be the ciphertext envelope produced by the `'e2ee'` session's `encrypt()` and co-enforced by the relay:

```js
{ type: 'chat', v: 1, /* group mode adds: ep; ratchet mode also adds: i; archive adds: a */ iv, ct }
```

A frame carrying a plaintext body field (`text`/`content`/`body`/`message`/`msg`/`plaintext`/`caption`/`note`) is **dropped** server-side with an `_type:'error'` - both when it has no `iv`/`ct` AND when it carries such a field ALONGSIDE the `{iv,ct}` envelope (never mix a readable body into an encrypted frame). Control/signaling frames (`e2ee-pub`, `e2ee-gkey`, WebRTC `offer`/`answer`/`ice`/`hello`/`bye`, presence/typing) pass in the clear because they carry no readable body. The relay persists only the ciphertext envelope (`iv`/`ct`, plus `type`/`v`/`ep`/`a` if present) - no client-supplied field beyond those.

**Blobs (files, images, voice) are NOT this envelope.** Raw binary frames are rejected, so send a blob through the `'e2ee'` session's `encryptBlob()`, which returns a DIFFERENT shape:

```js
{ meta: { v: 1, name, type, size, n /* group mode adds: ep; archive adds: a */ }, chunks: [ '<base64>', ... ] }
```

Each chunk is ONE opaque base64 string (the chunk's IV prepended to its ciphertext), not an `{iv,ct}` object - carry `meta` and `chunks` in a frame of your own naming and reassemble with `decryptBlob()`. Do not hand-roll blob chunks as top-level `{iv,ct}` frames: the relay would read them as chat envelopes and persist them, which is exactly what the chunk shape exists to avoid.
