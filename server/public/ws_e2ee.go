package public

// End-to-end-encryption relay enforcement. On an e2ee endpoint the server never
// holds the key; these helpers are the backstop that keeps cleartext message
// bodies off the relay and out of the durable table. The vetted js/e2ee.js helper
// is what actually guarantees messages are encrypted.

// e2eePlaintextBodyKeys are the field names that carry human-readable message
// content. On an e2ee endpoint a frame using any of these without ciphertext is
// the exact "forgot to encrypt the chat" leak we refuse to relay. Signaling and
// app control frames (e2ee-pub, offer/answer/ice, hello, bye, verify-confirm,
// typing, presence, …) carry no such field and pass untouched - blocking them
// would needlessly break legitimate cleartext coordination.
var e2eePlaintextBodyKeys = []string{"text", "content", "body", "message", "msg", "plaintext", "caption", "note"}

// e2eeEnvelopeKeys are the ONLY fields persisted from a frame on an e2ee
// endpoint: the ciphertext envelope plus its metadata. Every other client-supplied
// field is dropped before the durable write, so no cleartext can reach storage.
// "a" is the archive marker: the live message key is a per-session ephemeral, so a
// persisted row is only readable when the client encrypted it under the
// password-derived archive key (e2ee guide, archive:true). The marker rides the row
// so a later reader picks that key without guessing.
var e2eeEnvelopeKeys = []string{"iv", "ct", "type", "v", "ep", "a"}

// isE2EEEnvelope reports whether a parsed frame is the ciphertext envelope the
// browser helper emits: non-empty base64 iv and ct. The relay never inspects
// the plaintext - it only confirms the message is carried as ciphertext.
func isE2EEEnvelope(m map[string]any) bool {
	iv, _ := m["iv"].(string)
	ct, _ := m["ct"].(string)
	return iv != "" && ct != ""
}

// e2eeRatchetIndexKey is the per-message index the group helper stamps in ratchet
// mode ({type:'chat', v, ep, i, iv, ct}). It is deliberately NOT in e2eeEnvelopeKeys:
// a ratchet message key is derived, used, then destroyed for per-message forward
// secrecy, so a persisted ratchet row can never be re-keyed by a later reader.
const e2eeRatchetIndexKey = "i"

// isE2EERatchetEnvelope reports whether an e2ee envelope was produced in ratchet
// mode (carries the index i). A write_to_table endpoint that receives these is
// misconfigured: it would store rows nobody can ever decrypt. The relay uses this to
// refuse the durable write and tell the app, instead of silently banking dead rows.
// (For durable encrypted history the app uses archive:true; ratchet is for ephemeral
// rooms only - see the e2ee guide.)
func isE2EERatchetEnvelope(m map[string]any) bool {
	if !isE2EEEnvelope(m) {
		return false
	}
	_, ok := m[e2eeRatchetIndexKey]
	return ok
}

// e2eeFrameAllowed decides whether an inbound frame may be relayed/persisted on
// an e2ee endpoint. The ciphertext envelope always passes. Otherwise the frame is
// rejected if it carries a recognized plaintext message field, or if it claims the
// reserved chat type without being a real envelope - the two shapes of the "forgot
// to encrypt the chat" leak. Control and signaling frames (no readable body, not
// typed 'chat') pass, so the app's own cleartext coordination keeps working.
//
// This is a heuristic backstop over field names plus the reserved type, not a proof
// the ciphertext is well-formed; the vetted js/e2ee.js helper is what guarantees
// messages are actually encrypted. It is deliberately NOT an allowlist of frame
// types: encrypted blobs and WebRTC signaling ride app-NAMED frames (a 'file-chunk'
// carrying a base64 `data` string, an app-chosen typing/presence type), so a fixed
// type list would drop legitimate encrypted traffic. What can still slip past is
// readable text under a NON-'chat' type in a field the denylist does not name, which
// the composer avoids by sending through encrypt().
func e2eeFrameAllowed(m map[string]any) bool {
	// A frame is refused if it carries a readable plaintext body - whether or not
	// it ALSO presents an {iv,ct} envelope. Attaching dummy iv/ct must not smuggle
	// a cleartext "text"/"caption"/... field past the guard and into the relay or
	// the durable table.
	for _, k := range e2eePlaintextBodyKeys {
		if s, ok := m[k].(string); ok && s != "" {
			return false
		}
	}
	// The chat type is reserved for the ciphertext envelope. A type:'chat' frame
	// that is NOT a well-formed {iv,ct} envelope but still carries readable text in
	// ANY field is plaintext chat sent without encrypt() - drop it whatever the field
	// is named, since the denylist above only knows the common names. A bare or
	// empty-bodied chat frame carries nothing readable and still passes.
	if t, _ := m["type"].(string); t == "chat" && !isE2EEEnvelope(m) {
		for k, v := range m {
			if k == "type" {
				continue
			}
			if s, ok := v.(string); ok && s != "" {
				return false
			}
		}
	}
	return true
}
