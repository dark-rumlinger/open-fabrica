package public

import "testing"

// TestIsE2EEEnvelope pins what counts as the ciphertext envelope: both iv and
// ct present and non-empty.
func TestIsE2EEEnvelope(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want bool
	}{
		{"full envelope", map[string]any{"type": "chat", "iv": "AAAA", "ct": "BBBB"}, true},
		{"group envelope with ep", map[string]any{"type": "chat", "ep": 2, "iv": "AAAA", "ct": "BBBB"}, true},
		{"missing ct", map[string]any{"iv": "AAAA"}, false},
		{"missing iv", map[string]any{"ct": "BBBB"}, false},
		{"empty strings", map[string]any{"iv": "", "ct": ""}, false},
		{"plaintext chat", map[string]any{"type": "chat", "text": "hello"}, false},
		{"non-string ct", map[string]any{"iv": "AAAA", "ct": 5}, false},
		// A wrapped group-key frame is NOT an envelope: it carries no top-level
		// {iv,ct}, so the relay never persists it to write_to_table.
		{"e2ee-gkey not persisted", map[string]any{"type": "e2ee-gkey", "ep": 1, "by": "c-a", "wraps": map[string]any{"c-b": map[string]any{"iv": "AAAA", "ct": "BBBB"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isE2EEEnvelope(tc.m); got != tc.want {
				t.Errorf("isE2EEEnvelope(%v) = %v, want %v", tc.m, got, tc.want)
			}
		})
	}
}

// TestE2EEFrameAllowed pins the relay enforcement decision: ciphertext passes;
// a frame carrying a plaintext message body is dropped; control, signaling, and
// app coordination frames (which carry no readable body) pass untouched.
func TestE2EEFrameAllowed(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want bool
	}{
		{"ciphertext envelope", map[string]any{"type": "chat", "iv": "AAAA", "ct": "BBBB"}, true},
		{"group ciphertext envelope with ep", map[string]any{"type": "chat", "ep": 3, "iv": "AAAA", "ct": "BBBB"}, true},
		{"e2ee-gkey wrapped key passes", map[string]any{"type": "e2ee-gkey", "ep": 1, "by": "c-a", "wraps": map[string]any{"c-b": map[string]any{"iv": "AAAA", "ct": "BBBB"}}}, true},
		{"plaintext chat dropped", map[string]any{"type": "chat", "text": "hi"}, false},
		{"plaintext group chat dropped", map[string]any{"type": "chat", "ep": 1, "text": "hi"}, false},
		{"plaintext content dropped", map[string]any{"type": "message", "content": "secret"}, false},
		{"plaintext body dropped", map[string]any{"type": "note", "body": "secret"}, false},
		{"empty text passes", map[string]any{"type": "chat", "text": ""}, true},
		{"e2ee-pub handshake", map[string]any{"type": "e2ee-pub", "pub": "xxx"}, true},
		{"e2ee-pub signed (idpub+sig) passes", map[string]any{"type": "e2ee-pub", "pub": "xxx", "idpub": "yyy", "sig": "zzz"}, true},
		{"webrtc offer", map[string]any{"type": "offer", "sdp": "v=0..."}, true},
		{"webrtc answer", map[string]any{"type": "answer", "sdp": "v=0..."}, true},
		{"webrtc ice", map[string]any{"type": "ice", "candidate": "..."}, true},
		{"hello", map[string]any{"type": "hello", "id": "abc"}, true},
		{"bye", map[string]any{"type": "bye"}, true},
		{"verify-confirm passes", map[string]any{"type": "verify-confirm"}, true},
		{"typing indicator passes", map[string]any{"type": "typing"}, true},
		{"room switch passes", map[string]any{"room": "lobby"}, true},
		// The reserved chat type: a type:'chat' frame that is not a real envelope but
		// carries readable text in ANY field is a plaintext chat leak, even under a
		// field name the denylist does not list.
		{"chat type with unlisted content field dropped", map[string]any{"type": "chat", "line": "secret"}, false},
		{"bare chat type passes", map[string]any{"type": "chat"}, true},
		{"empty-bodied chat type passes", map[string]any{"type": "chat", "text": ""}, true},
		// The expanded denylist catches common content field names under any type.
		{"caption body dropped", map[string]any{"type": "reaction", "caption": "secret"}, false},
		{"note body dropped", map[string]any{"type": "post", "note": "secret"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e2eeFrameAllowed(tc.m); got != tc.want {
				t.Errorf("e2eeFrameAllowed(%v) = %v, want %v", tc.m, got, tc.want)
			}
		})
	}
}
