package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"openfabrica/internal/httpx"
	"openfabrica/internal/safego"
)

const maxChatBodySize = 1 << 20 // 1 MB

const sessionMaxIdle = 2 * time.Hour

// sessionEntry pairs a session with its last-used timestamp and a reference
// count to prevent cleanup during active streaming.
type sessionEntry struct {
	session  *Session
	lastUsed time.Time
	inUse    int32 // atomic ref count of active streams
}

// ChatHandler exposes HTTP endpoints for the streaming chat interface.
type ChatHandler struct {
	deps     SessionDeps
	mu       sync.Mutex
	sessions map[string]*sessionEntry // keyed by "projectID:sessionID"
	stop     chan struct{}            // closed by Close to end the cleanup loop
}

// NewChatHandler creates a ChatHandler with the given dependencies.
func NewChatHandler(deps SessionDeps) *ChatHandler {
	ch := &ChatHandler{
		deps:     deps,
		sessions: make(map[string]*sessionEntry),
		stop:     make(chan struct{}),
	}
	safego.Go("chat-session-cleanup", ch.cleanupLoop)
	return ch
}

// Close stops the background cleanup loop. Safe to call once at shutdown.
func (h *ChatHandler) Close() {
	close(h.stop)
}

// cleanupLoop removes idle sessions every 30 minutes until Close is called.
func (h *ChatHandler) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
		}
		h.mu.Lock()
		now := time.Now()
		for key, entry := range h.sessions {
			if atomic.LoadInt32(&entry.inUse) == 0 && now.Sub(entry.lastUsed) > sessionMaxIdle {
				delete(h.sessions, key)
			}
		}
		h.mu.Unlock()
	}
}

// sendRequest is the JSON body sent by the client to the send endpoint.
type sendRequest struct {
	Message string `json:"message"`
}

// writeChatError sends the shared {"error": msg} envelope (internal/httpx) so
// the chat endpoints report failures in exactly the same wire shape as the
// admin and public APIs, instead of carrying a local copy that can drift.
func writeChatError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteError(w, status, msg)
}

// HandleSend is the synchronous chat endpoint. Reads a user message, runs the
// LLM tool-call loop to completion, and returns the final assistant text plus
// per-tool activity as a single JSON response.
//
// Expected request: POST with JSON body {"message":"..."} and query params
// project_id and session_id.
func (h *ChatHandler) HandleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeChatError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	projectIDStr := r.URL.Query().Get("project_id")
	sessionID := r.URL.Query().Get("session_id")
	if projectIDStr == "" || sessionID == "" {
		writeChatError(w, http.StatusBadRequest, "project_id and session_id query parameters are required")
		return
	}

	var projectID int
	if _, err := fmt.Sscanf(projectIDStr, "%d", &projectID); err != nil {
		writeChatError(w, http.StatusBadRequest, "project_id must be an integer")
		return
	}

	var req sendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxChatBodySize)).Decode(&req); err != nil {
		writeChatError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Message == "" {
		writeChatError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Open creates a database for any id it is given; a stale tab chatting into a
	// deleted project must get a 404, not a freshly recreated project directory.
	if !h.deps.ProjectDBManager.Exists(projectID) {
		writeChatError(w, http.StatusNotFound, "project not found")
		return
	}

	entry, err := h.getOrCreateSession(projectID, sessionID)
	if err != nil {
		writeChatError(w, http.StatusInternalServerError, "failed to open project database")
		return
	}
	atomic.AddInt32(&entry.inUse, 1)
	defer atomic.AddInt32(&entry.inUse, -1)
	session := entry.session
	logger := h.deps.Logger.With("handler", "chat_send", "project_id", projectID, "session_id", sessionID)

	// A reply can take minutes (a tool loop). The admin server's WriteTimeout is sized
	// for ordinary requests and would cut the connection before the answer is written,
	// so the caller would see a network error for work that finished.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) // best-effort; some writers have no deadline

	result, sendErr := session.Send(r.Context(), req.Message)
	if sendErr != nil {
		logger.Error("session send error", "error", sendErr)
		writeChatError(w, http.StatusInternalServerError, sendErr.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		logger.Error("encode send result", "error", err)
	}
}

// HandleHistory returns the chat history for a session as JSON.
// GET with query params project_id and session_id.
func (h *ChatHandler) HandleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeChatError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	projectIDStr := r.URL.Query().Get("project_id")
	sessionID := r.URL.Query().Get("session_id")
	if projectIDStr == "" || sessionID == "" {
		writeChatError(w, http.StatusBadRequest, "project_id and session_id query parameters are required")
		return
	}

	var projectID int
	if _, err := fmt.Sscanf(projectIDStr, "%d", &projectID); err != nil {
		writeChatError(w, http.StatusBadRequest, "project_id must be an integer")
		return
	}

	// Parse optional before parameter for pagination. It is the row id of the
	// oldest message the client currently holds; the next page is everything with
	// a smaller id. Non-numeric or absent means "load the newest page".
	beforeStr := r.URL.Query().Get("before")
	var beforeID int64
	if beforeStr != "" {
		beforeID, _ = strconv.ParseInt(beforeStr, 10, 64)
	}

	// Open creates a database for any id it is given; see HandleSend.
	if !h.deps.ProjectDBManager.Exists(projectID) {
		writeChatError(w, http.StatusNotFound, "project not found")
		return
	}

	// Open the project DB for history queries.
	projectDB, err := h.deps.ProjectDBManager.Open(projectID)
	if err != nil {
		h.deps.Logger.Error("failed to open project db", "project_id", projectID, "error", err)
		writeChatError(w, http.StatusInternalServerError, "failed to open project database")
		return
	}

	// For the admin session, return merged history (admin + engine messages)
	// so the chat UI shows engine activity alongside user-initiated chat.
	// Initial load (no `before` param) returns a timeline payload that also
	// includes stage transitions, the latest validation report, and the
	// pipeline cursor. Without these the rehydrated view drifts from the
	// live one. Paginated loads return only messages, since stages and
	// validation are not paginated.
	if sessionID == "admin" {
		if beforeID <= 0 {
			tl, err := LoadTimeline(projectDB.Reader(), historyLimit)
			if err != nil {
				h.deps.Logger.Error("load timeline error", "error", err)
				writeChatError(w, http.StatusInternalServerError, "failed to load history")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(tl); err != nil {
				h.deps.Logger.Error("encode timeline error", "error", err)
			}
			return
		}

		merged, err := LoadMergedHistory(projectDB.Reader(), historyLimit, beforeID)
		if err != nil {
			h.deps.Logger.Error("load merged history error", "error", err)
			writeChatError(w, http.StatusInternalServerError, "failed to load history")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(merged); err != nil {
			h.deps.Logger.Error("encode merged history error", "error", err)
		}
		return
	}

	messages, err := LoadHistory(projectDB.Reader(), sessionID, historyLimit)
	if err != nil {
		h.deps.Logger.Error("load history error", "error", err)
		writeChatError(w, http.StatusInternalServerError, "failed to load history")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(messages); err != nil {
		h.deps.Logger.Error("encode history error", "error", err)
	}
}

// getOrCreateSession returns an existing session or creates a new one.
func (h *ChatHandler) getOrCreateSession(projectID int, sessionID string) (*sessionEntry, error) {
	key := fmt.Sprintf("%d:%s", projectID, sessionID)

	h.mu.Lock()
	defer h.mu.Unlock()

	if entry, ok := h.sessions[key]; ok {
		entry.lastUsed = time.Now()
		return entry, nil
	}

	s, err := NewSession(sessionID, projectID, h.deps)
	if err != nil {
		return nil, err
	}
	entry := &sessionEntry{session: s, lastUsed: time.Now()}
	h.sessions[key] = entry
	return entry, nil
}
