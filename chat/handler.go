package chat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"openfabrica/internal/httpx"
	"openfabrica/providers/llm"
)

const (
	maxChatBodySize = 16 << 20 // 16 MB
	maxImageCount   = 4
	maxImageBytes   = 4 << 20 // 4 MB per decoded image
)

// ChatHandler exposes HTTP endpoints for the chat interface.
type ChatHandler struct {
	deps SessionDeps
}

// NewChatHandler creates a ChatHandler with the given dependencies.
func NewChatHandler(deps SessionDeps) *ChatHandler {
	return &ChatHandler{deps: deps}
}

// Close is a no-op retained for shutdown lifecycle compatibility.
func (h *ChatHandler) Close() {}

// ImageInput is a base64-encoded image attachment in a chat or project request.
type ImageInput struct {
	MIMEType string `json:"mime_type"`
	Data     string `json:"data"`
}

// DecodeImageInputs validates and decodes up to maxImageCount base64 images.
func DecodeImageInputs(inputs []ImageInput) ([]llm.MessageImage, error) {
	if len(inputs) > maxImageCount {
		return nil, fmt.Errorf("at most %d images are allowed", maxImageCount)
	}
	var out []llm.MessageImage
	for _, in := range inputs {
		mime := strings.ToLower(strings.TrimSpace(in.MIMEType))
		switch mime {
		case "image/png", "image/jpeg", "image/webp", "image/gif":
		default:
			return nil, fmt.Errorf("unsupported image type %q", in.MIMEType)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.Data))
		if err != nil {
			return nil, fmt.Errorf("invalid base64 image data: %w", err)
		}
		if len(raw) == 0 || len(raw) > maxImageBytes {
			return nil, fmt.Errorf("image size must be between 1 byte and %d bytes", maxImageBytes)
		}
		out = append(out, llm.MessageImage{MIMEType: mime, Data: raw})
	}
	return out, nil
}

// sendRequest is the JSON body sent by the client to the send endpoint.
type sendRequest struct {
	Message string       `json:"message"`
	Images  []ImageInput `json:"images,omitempty"`
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
	if strings.TrimSpace(req.Message) == "" && len(req.Images) == 0 {
		writeChatError(w, http.StatusBadRequest, "message is required")
		return
	}
	images, err := DecodeImageInputs(req.Images)
	if err != nil {
		writeChatError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		req.Message = "Please inspect the attached image(s)."
	}

	// Open creates a database for any id it is given; a stale tab chatting into a
	// deleted project must get a 404, not a freshly recreated project directory.
	if !h.deps.ProjectDBManager.Exists(projectID) {
		writeChatError(w, http.StatusNotFound, "project not found")
		return
	}

	session, err := NewSession(sessionID, projectID, h.deps)
	if err != nil {
		writeChatError(w, http.StatusInternalServerError, "failed to open project database")
		return
	}
	logger := h.deps.Logger.With("handler", "chat_send", "project_id", projectID, "session_id", sessionID)

	result, sendErr := session.SendWithImages(r.Context(), req.Message, images)
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
