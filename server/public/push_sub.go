package public

import (
	"encoding/json"
	"net/http"
	"strings"

	"openfabrica/db"
)

// apiPushSubscribe handles POST /api/<path>/push/subscribe. Body is the raw
// PushSubscription object from the browser:
//
//	{"endpoint":"https://...","keys":{"p256dh":"...","auth":"..."}}
//
// Saves it under of_push_subscriptions keyed by endpoint_url.
func (h *Handler) apiPushSubscribe(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB, path string) {
	var exists int
	if err := projectDB.Reader().QueryRow("SELECT COUNT(*) FROM of_push_endpoints WHERE path = ?", path).Scan(&exists); err != nil || exists == 0 {
		writeError(w, http.StatusNotFound, "push endpoint not found")
		return
	}

	var requiresAuth bool
	_ = projectDB.Reader().QueryRow("SELECT requires_auth FROM of_push_endpoints WHERE path = ?", path).Scan(&requiresAuth) // existence checked above; a miss keeps requiresAuth false

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
		// Opportunistically capture user_id if a valid token is present.
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
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64 KB: a push subscription is tiny
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if body.Endpoint == "" || body.Keys.P256DH == "" || body.Keys.Auth == "" {
		writeError(w, http.StatusBadRequest, "endpoint, keys.p256dh, keys.auth are required")
		return
	}

	var uidArg any
	if userID > 0 {
		uidArg = userID
	}

	_, err := projectDB.Writer().Exec(`
		INSERT INTO of_push_subscriptions (endpoint_path, user_id, endpoint_url, p256dh, auth_key)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint_url) DO UPDATE SET
		  p256dh = excluded.p256dh,
		  auth_key = excluded.auth_key,
		  user_id = excluded.user_id`,
		path, uidArg, body.Endpoint, body.Keys.P256DH, body.Keys.Auth,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save subscription: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}
