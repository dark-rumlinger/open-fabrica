---
id: e2ee-security-model
title: Security model for private/encrypted realtime briefs
kind: prose
governs: []
probes: []
---
**Security model for a private / encrypted realtime brief (read before choosing tables and endpoints).** The platform supports browser-native end-to-end encryption (`e2ee=true` on a `manage_relay` chat or `manage_signaling` call endpoint) in which the shared PASSWORD is the client-only encryption key. This model applies ONLY when the brief's password is a shared room/conversation key (a private room joined by password, an encrypted chat or call); if the brief instead demands user ACCOUNTS (register/login, roles, admin powers, per-user server data), it is NOT this shape - use standard platform auth (`create_auth`), where a server-hashed `PASSWORD` column is correct. For a genuinely encrypted brief, design around this model:

- Do NOT create a `password`, `password_hash`, `passcode`, or `secret` column on any table, and do NOT add an endpoint, lookup, or task for "password verification". The password must NEVER reach the server - verifying it server-side breaks E2E and is the single most common way these apps are secretly broken.
- A room "exists"/join check needs only the room name or code; the password is carried client-side only and fed straight into the encryption helper.
- If entry itself must be gated - especially for video/voice CALLS, where the peer-to-peer media is NOT protected by the password - gate it with a SEPARATE high-entropy access token (`room_gated`), never the password.
- Put `e2ee`, `realtime_signaling`, and (for video/voice calls) `webrtc_signaling` in `guides` so the full contract loads at BUILD.
