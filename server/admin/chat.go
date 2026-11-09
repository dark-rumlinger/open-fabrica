package admin

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// ChatHandlerAdmin wraps the chat.ChatHandler for admin routes.
type ChatHandlerAdmin struct {
	deps *Deps
}

// prepChatRequest validates the {projectID} route param and injects project_id
// plus a default session_id into the query so the shared chat.ChatHandler can
// serve an admin request. Writes a 400 and returns false on a malformed id.
func prepChatRequest(w http.ResponseWriter, r *http.Request) bool {
	projectID := chi.URLParam(r, "projectID")
	if _, err := strconv.Atoi(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return false
	}
	q := r.URL.Query()
	q.Set("project_id", projectID)
	if q.Get("session_id") == "" {
		q.Set("session_id", "admin")
	}
	r.URL.RawQuery = q.Encode()
	return true
}

// Send delegates to ChatHandler.HandleSend after injecting the projectID as a
// query parameter. The response is plain JSON, not SSE.
func (h *ChatHandlerAdmin) Send(w http.ResponseWriter, r *http.Request) {
	if !prepChatRequest(w, r) {
		return
	}
	// The send publishes the event that wakes the engine, so the language goes in first.
	if id, err := strconv.Atoi(chi.URLParam(r, "projectID")); err == nil {
		h.deps.recordReplyLanguage(r, id)
	}
	h.deps.ChatHandler.HandleSend(w, r)
}

// History delegates to ChatHandler.HandleHistory.
func (h *ChatHandlerAdmin) History(w http.ResponseWriter, r *http.Request) {
	if !prepChatRequest(w, r) {
		return
	}
	h.deps.ChatHandler.HandleHistory(w, r)
}
