package chat

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
)

// Chatting into, or loading the history of, a project that does not exist must
// answer 404. Opening its database would create the directory and an empty file, so
// a stale tab polling after a delete would resurrect a ghost project on disk.
func TestChatEndpointsRefuseAMissingProject(t *testing.T) {
	dataDir := t.TempDir()
	pm := db.NewProjectDBManager(dataDir)
	t.Cleanup(func() { pm.CloseAll() })
	h := NewChatHandler(SessionDeps{
		ProjectDBManager: pm,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(h.Close)

	rec := httptest.NewRecorder()
	h.HandleHistory(rec, httptest.NewRequest(http.MethodGet, "/chat/history?project_id=41&session_id=s1", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("history of a missing project: status %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send?project_id=41&session_id=s1", strings.NewReader(`{"message":"hello"}`))
	h.HandleSend(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("send to a missing project: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dataDir, "projects", "41")); err == nil {
		t.Error("a chat call against a missing project created its directory")
	}
}
