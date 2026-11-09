package tools

import (
	"strings"
	"testing"

	"openfabrica/store"
)

// The endpoint-path rejection and the page-path rejection (in tools/store) must
// both reference the SAME store.PathParamNote, so the two path grammars are taught
// as one coherent rule instead of two contradictory halves. This pins the endpoint
// side; TestPagePathParamRejectionsShareTheNote pins the page side.
func TestEndpointPathRejectionCarriesSharedNote(t *testing.T) {
	msg := ValidatePathSegments("collab/:roomId")
	if msg == "" {
		t.Fatal("expected a :param endpoint-path rejection")
	}
	if !strings.Contains(msg, store.PathParamNote) {
		t.Errorf("endpoint-path rejection must carry the shared store.PathParamNote; got: %q", msg)
	}
}

// TestEndpointPathRejectionDropsRoomGuidance guards against websocket/room advice
// being appended to EVERY :param rejection, including manage_function paths where
// "scope per-room / room_column" is nonsensical. The message must stay generic to
// path parameters.
func TestEndpointPathRejectionDropsRoomGuidance(t *testing.T) {
	msg := ValidatePathSegments("files/:slug/download")
	if strings.Contains(strings.ToLower(msg), "room") {
		t.Errorf("path-param rejection should not carry websocket/room guidance; got: %q", msg)
	}
}
