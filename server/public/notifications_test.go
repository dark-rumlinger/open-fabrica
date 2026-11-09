package public

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db/models"
	"openfabrica/security"
)

type notifRig struct {
	h   *Handler
	jwt *security.JWTManager
	r   chi.Router
}

func newNotifRig(t *testing.T) *notifRig {
	t.Helper()
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_notifications (user_id, title, message, type) VALUES (5, 'Welcome', 'hi', 'info')`)
	mustExec(t, w, `INSERT INTO of_notifications (user_id, title, message, type) VALUES (5, 'Order shipped', '', 'success')`)
	mustExec(t, w, `INSERT INTO of_notifications (user_id, title, message, type) VALUES (6, 'Not yours', 'secret', 'info')`)
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr, JWTManager: jwt}}
	r := chi.NewRouter()
	r.Get("/__of/notifications", h.NotificationsList)
	r.Get("/__of/notifications/count", h.NotificationsCount)
	r.Post("/__of/notifications/read-all", h.NotificationsReadAll)
	r.Post("/__of/notifications/{id}/read", h.NotificationsMarkRead)
	r.Delete("/__of/notifications/{id}", h.NotificationsDelete)
	return &notifRig{h: h, jwt: jwt, r: r}
}

func (n *notifRig) call(t *testing.T, method, path string, userID int) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if userID != 0 {
		tok, err := n.jwt.Generate(security.TokenRequest{ProjectID: 1, UserID: userID, Username: "u", Role: "user"})
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
	rec := httptest.NewRecorder()
	n.r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestNotificationsAreScopedToTheCaller(t *testing.T) {
	n := newNotifRig(t)

	if code, _ := n.call(t, "GET", "/__of/notifications", 0); code != http.StatusUnauthorized {
		t.Errorf("anonymous list: %d, want 401", code)
	}
	code, out := n.call(t, "GET", "/__of/notifications", 5)
	if code != 200 || out["count"] != float64(2) || out["unread"] != float64(2) {
		t.Fatalf("list for user 5: %d %v", code, out)
	}
	for _, row := range out["rows"].([]any) {
		if row.(map[string]any)["title"] == "Not yours" {
			t.Fatal("another user's notification was listed")
		}
	}
}

func TestNotificationsMarkReadAndDelete(t *testing.T) {
	n := newNotifRig(t)
	_, list := n.call(t, "GET", "/__of/notifications", 5)
	first := int(list["rows"].([]any)[0].(map[string]any)["id"].(float64))

	if code, out := n.call(t, "POST", "/__of/notifications/"+itoa(first)+"/read", 5); code != 200 || out["unread"] != float64(1) {
		t.Fatalf("mark read: %d %v", code, out)
	}
	if code, out := n.call(t, "GET", "/__of/notifications?unread=1", 5); code != 200 || out["count"] != float64(1) {
		t.Fatalf("unread filter: %d %v", code, out)
	}
	if code, _ := n.call(t, "POST", "/__of/notifications/"+itoa(first)+"/read", 6); code != http.StatusNotFound {
		t.Errorf("another user marking it read: %d, want 404", code)
	}
	if code, _ := n.call(t, "DELETE", "/__of/notifications/"+itoa(first), 6); code != http.StatusNotFound {
		t.Errorf("another user deleting it: %d, want 404", code)
	}
	if code, out := n.call(t, "POST", "/__of/notifications/read-all", 5); code != 200 || out["unread"] != float64(0) {
		t.Fatalf("read-all: %d %v", code, out)
	}
	if code, _ := n.call(t, "DELETE", "/__of/notifications/"+itoa(first), 5); code != 200 {
		t.Errorf("owner delete: %d", code)
	}
	if code, out := n.call(t, "GET", "/__of/notifications/count", 5); code != 200 || out["unread"] != float64(0) {
		t.Errorf("count: %d %v", code, out)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
