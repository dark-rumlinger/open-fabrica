package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The admin chat recognises the expanded brief by the same bytes the engine
// writes. If either side changes alone, the brief silently renders as a plain
// bubble with the marker showing, so the two are pinned together here.
func TestExpandedBriefMarkerMatchesTheChatUI(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "web", "admin", "app", "ui", "chat", "message.js"))
	if err != nil {
		t.Fatalf("read chat message source: %v", err)
	}
	if want := "'" + ExpandedBriefMarker + "'"; !strings.Contains(string(src), want) {
		t.Errorf("message.js does not match the engine's marker %s", want)
	}
}
