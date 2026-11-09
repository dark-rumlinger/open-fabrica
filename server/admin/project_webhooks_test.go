package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The admin webhook API creates, lists, reads and re-secrets webhooks without the
// signing secret ever sitting in the database in plain text or being echoed back by
// a list or get; it is returned once, at creation.
func TestAdminWebhookSecretLifecycle(t *testing.T) {
	d := newBotTestDeps(t)
	pdb, err := d.ProjectDBManager.Open(5)
	if err != nil {
		t.Fatal(err)
	}
	h := &ProjectWebhooksHandler{deps: d}
	proj := map[string]string{"projectID": "5"}

	rec := doJSON(h.Create, "POST", "/projects/5/webhooks", `{"name":"orders","direction":"incoming"}`, proj)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     int    `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || len(created.Secret) < 32 {
		t.Fatalf("create returned no usable secret: %v %s", err, rec.Body.String())
	}

	dbSecret := func() string {
		var stored string
		if err := pdb.QueryRow(`SELECT secret_encrypted FROM of_webhooks WHERE name = 'orders'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}
	stored := dbSecret()
	if stored == "" || strings.Contains(stored, created.Secret) {
		t.Fatalf("the secret is not stored as ciphertext: %q", stored)
	}
	if opened, err := d.Encryptor.OpenOptional(stored); err != nil || opened != created.Secret {
		t.Fatalf("stored value does not decrypt to the returned secret: %q, %v", opened, err)
	}

	// List and get report that a secret exists, never what it is.
	for name, call := range map[string]func() *strings.Builder{
		"list": func() *strings.Builder {
			var b strings.Builder
			b.WriteString(doJSON(h.List, "GET", "/projects/5/webhooks", "", proj).Body.String())
			return &b
		},
		"get": func() *strings.Builder {
			var b strings.Builder
			b.WriteString(doJSON(h.Get, "GET", "/projects/5/webhooks/1", "", map[string]string{"projectID": "5", "webhookID": "1"}).Body.String())
			return &b
		},
	} {
		body := call().String()
		if !strings.Contains(body, `"has_secret":true`) {
			t.Errorf("%s does not report that a secret is set: %s", name, body)
		}
		if strings.Contains(body, created.Secret) || strings.Contains(body, stored) {
			t.Errorf("%s leaked the secret or its ciphertext: %s", name, body)
		}
	}

	// Replacing the secret seals the new one; clearing it leaves the column empty.
	upd := map[string]string{"projectID": "5", "webhookID": "1"}
	if rec := doJSON(h.Update, "PUT", "/projects/5/webhooks/1", `{"secret":"replacement-secret"}`, upd); rec.Code != http.StatusOK {
		t.Fatalf("update secret: status %d, body %s", rec.Code, rec.Body.String())
	}
	next := dbSecret()
	if next == stored || strings.Contains(next, "replacement-secret") {
		t.Errorf("the replaced secret was not re-sealed: %q", next)
	}
	if opened, _ := d.Encryptor.OpenOptional(next); opened != "replacement-secret" {
		t.Errorf("replaced secret decrypts to %q", opened)
	}
	if rec := doJSON(h.Update, "PUT", "/projects/5/webhooks/1", `{"secret":""}`, upd); rec.Code != http.StatusOK {
		t.Fatalf("clear secret: status %d", rec.Code)
	}
	if got := dbSecret(); got != "" {
		t.Errorf("a cleared secret left %q in the column", got)
	}
	body := doJSON(h.List, "GET", "/projects/5/webhooks", "", proj).Body.String()
	if !strings.Contains(body, `"has_secret":false`) {
		t.Errorf("list does not show the cleared secret: %s", body)
	}
}

// The test endpoint sends from the server, so it must refuse what a real
// delivery refuses (private targets), reject incoming webhooks (they receive
// requests, they do not send them), and answer 404 for an unknown id.
func TestAdminWebhookTestEndpoint(t *testing.T) {
	d := newBotTestDeps(t)
	pdb, err := d.ProjectDBManager.Open(5)
	if err != nil {
		t.Fatal(err)
	}
	h := &ProjectWebhooksHandler{deps: d}

	if _, err := pdb.ExecWrite(`INSERT INTO of_webhooks (name, direction, url, secret_encrypted) VALUES ('in', 'incoming', NULL, '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_webhooks (name, direction, url, secret_encrypted) VALUES ('out', 'outgoing', 'http://127.0.0.1:9/hook', '')`); err != nil {
		t.Fatal(err)
	}
	var inID, outID string
	for name, dst := range map[string]*string{"in": &inID, "out": &outID} {
		if err := pdb.QueryRow(`SELECT CAST(id AS TEXT) FROM of_webhooks WHERE name = ?`, name).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}

	call := func(id string) (int, string) {
		rec := doJSON(h.Test, "POST", "/projects/5/webhooks/"+id+"/test", "", map[string]string{"projectID": "5", "webhookID": id})
		return rec.Code, rec.Body.String()
	}

	if code, body := call("9999"); code != http.StatusNotFound {
		t.Errorf("unknown webhook: status %d, body %s, want 404", code, body)
	}
	if code, body := call(inID); code != http.StatusBadRequest {
		t.Errorf("incoming webhook: status %d, body %s, want 400", code, body)
	}
	code, body := call(outID)
	if code != http.StatusOK || !strings.Contains(body, `"success":false`) {
		t.Errorf("loopback target: status %d, body %s, want 200 with success=false (SSRF guard)", code, body)
	}
}
