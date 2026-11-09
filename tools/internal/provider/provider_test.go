package provider

import (
	"database/sql"
	"encoding/base64"
	"net/http"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/security"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func newTestEncryptor(t *testing.T) *security.Encryptor {
	t.Helper()
	enc, err := security.NewEncryptor("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	return enc
}

// setupSecretDB returns an in-memory DB holding one encrypted secret named
// "api_secret" whose plaintext is "tok-123".
func setupSecretDB(t *testing.T, enc *security.Encryptor) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	encrypted, err := enc.Encrypt("tok-123")
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	if _, err := db.Exec("INSERT INTO of_secrets (name, value_encrypted) VALUES ('api_secret', ?)", encrypted); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	return db
}

// TestInjectAuth pins the auth-header shape per auth_type. The basic cases pin the
// encoding: the header value must be base64, never the raw secret.
func TestInjectAuth(t *testing.T) {
	enc := newTestEncryptor(t)
	db := setupSecretDB(t, enc)
	ctx := &tools.ToolContext{DB: db, Encryptor: enc}

	secretRef := sql.NullString{String: "api_secret", Valid: true}

	cases := []struct {
		name      string
		creds     *Creds
		basicBase string
		header    string // header we expect to be set
		want      string // expected value; "" means header must be absent
	}{
		{
			name:   "bearer",
			creds:  &Creds{AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer", SecretName: secretRef},
			header: "Authorization",
			want:   "Bearer tok-123",
		},
		{
			name:   "api_key_header",
			creds:  &Creds{AuthType: "api_key_header", AuthHeader: "X-API-Key", SecretName: secretRef},
			header: "X-API-Key",
			want:   "tok-123",
		},
		{
			name:   "basic without base",
			creds:  &Creds{AuthType: "basic", AuthHeader: "Authorization", SecretName: secretRef},
			header: "Authorization",
			want:   "Basic " + base64.StdEncoding.EncodeToString([]byte("tok-123")),
		},
		{
			name:      "basic with base (Twilio shape)",
			creds:     &Creds{AuthType: "basic", AuthHeader: "Authorization", SecretName: secretRef},
			basicBase: "AC-sid",
			header:    "Authorization",
			want:      "Basic " + base64.StdEncoding.EncodeToString([]byte("AC-sid:tok-123")),
		},
		{
			name:   "none sets no header",
			creds:  &Creds{AuthType: "none", AuthHeader: "Authorization"},
			header: "Authorization",
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "https://example.com", nil)
			if r := InjectAuth(ctx, req, tc.creds, tc.basicBase); r != nil {
				t.Fatalf("InjectAuth returned failure: %+v", r)
			}
			got := req.Header.Get(tc.header)
			if got != tc.want {
				t.Errorf("%s header = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}
