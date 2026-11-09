package public

import (
	"testing"

	"openfabrica/internal/roomactor"
)

// Regression for the envelope-smuggling bypass: a frame that presents a dummy
// {iv,ct} envelope AND a cleartext body key must be refused. The prior guard
// short-circuited to "allowed" the instant iv+ct were present, so plaintext rode
// the relay (and got persisted) alongside fake ciphertext. (The broader guard
// behavior is covered by TestE2EEFrameAllowed in e2ee_relay_test.go.)
func TestE2EEFrameAllowedRejectsSmuggledPlaintext(t *testing.T) {
	cases := []struct {
		name  string
		frame map[string]any
		want  bool
	}{
		{"envelope only", map[string]any{"iv": "aa", "ct": "bb"}, true},
		{"envelope + smuggled text", map[string]any{"iv": "aa", "ct": "bb", "text": "secret"}, false},
		{"envelope + smuggled message", map[string]any{"iv": "aa", "ct": "bb", "message": "secret"}, false},
		{"envelope + smuggled body", map[string]any{"iv": "aa", "ct": "bb", "body": "secret"}, false},
		{"envelope + empty text still ok", map[string]any{"iv": "aa", "ct": "bb", "text": ""}, true},
	}
	for _, tc := range cases {
		if got := e2eeFrameAllowed(tc.frame); got != tc.want {
			t.Errorf("%s: e2eeFrameAllowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// isE2EERatchetEnvelope flags a persisted-ratchet misconfiguration: a ratchet
// envelope (carries the index i) written to a write_to_table endpoint stores a row
// whose message key is already destroyed, so it can never be reopened. The relay
// uses this to refuse the durable write. Only a real envelope carrying i qualifies;
// a bare i (no ciphertext) or an ordinary envelope must not.
func TestIsE2EERatchetEnvelope(t *testing.T) {
	cases := []struct {
		name  string
		frame map[string]any
		want  bool
	}{
		{"ratchet envelope", map[string]any{"type": "chat", "v": 1, "ep": 0, "i": 3, "iv": "aa", "ct": "bb"}, true},
		{"group envelope no i", map[string]any{"type": "chat", "v": 1, "ep": 0, "iv": "aa", "ct": "bb"}, false},
		{"two-party envelope", map[string]any{"type": "chat", "v": 1, "iv": "aa", "ct": "bb"}, false},
		{"i without ciphertext is not an envelope", map[string]any{"type": "chat", "i": 3}, false},
		{"i with empty iv/ct", map[string]any{"i": 3, "iv": "", "ct": ""}, false},
	}
	for _, tc := range cases {
		if got := isE2EERatchetEnvelope(tc.frame); got != tc.want {
			t.Errorf("%s: isE2EERatchetEnvelope = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// touchesSecureColumn must fold case: SQLite writes "Password" to the physical
// "password" column, so a reducer must not dodge the PASSWORD/ENCRYPTED guard
// (which prevents storing a raw secret) by varying the column's case.
func TestTouchesSecureColumnCaseInsensitive(t *testing.T) {
	secure := map[string]string{"password": "hash", "ssn": "encrypt"}
	cases := []struct {
		name string
		row  map[string]any
		want bool
	}{
		{"exact", map[string]any{"password": "x"}, true},
		{"uppercase", map[string]any{"PASSWORD": "x"}, true},
		{"mixed", map[string]any{"Password": "x"}, true},
		{"encrypt kind mixed", map[string]any{"Ssn": "x"}, true},
		{"non-secure", map[string]any{"name": "x"}, false},
	}
	for _, tc := range cases {
		w := roomactor.Write{Op: "update", Table: "users", Row: tc.row}
		if got := touchesSecureColumn(w, secure); got != tc.want {
			t.Errorf("%s: touchesSecureColumn = %v, want %v", tc.name, got, tc.want)
		}
	}
}
