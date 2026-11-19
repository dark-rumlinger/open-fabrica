package public

import (
	"encoding/json"
	"net/http"
	"strings"

	"openfabrica/db"
	"openfabrica/store"
)

// apiPushSubscribe handles POST /api/<path>/push/subscribe.
func (h *Handler) apiPushSubscribe(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB, path string) {
	requiresAuth, exists := store.PushEndpointRequiresAuth(projectDB, path)
	if !exists {
		writeError(w, http.StatusNotFound, "push endpoint not found")
		return
	}

	var userID int
	if requiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if h.deps.JWTManager == nil || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		claims, err := h.validateUserClaims(token)
		if err != nil || claims.ProjectID != projectID {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		userID = claims.UserID
	} else if h.deps.JWTManager != nil {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "" {
			if claims, err := h.validateUserClaims(token); err == nil && claims.ProjectID == projectID {
				userID = claims.UserID
			}
		}
	}

	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if body.Endpoint == "" || body.Keys.P256DH == "" || body.Keys.Auth == "" {
		writeError(w, http.StatusBadRequest, "endpoint, keys.p256dh, keys.auth are required")
		return
	}

	if err := store.UpsertPushSubscription(projectDB, path, userID, body.Endpoint, body.Keys.P256DH, body.Keys.Auth); err != nil {
		writeError(w, http.StatusInternalServerError, "save subscription: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}
