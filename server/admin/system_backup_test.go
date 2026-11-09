package admin

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"openfabrica/config"
	sdeps "openfabrica/server/deps"
)

// The backup endpoint streams a zip that holds the core database, the project
// database and the manifest, and names the file as a download.
func TestBackupDownloadStreamsArchive(t *testing.T) {
	dir := t.TempDir()
	mk := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		d, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
			t.Fatal(err)
		}
	}
	core := filepath.Join(dir, "openfabrica.db")
	mk(core)
	mk(filepath.Join(dir, "projects", "3", "project.db"))

	h := &SystemHandler{deps: &Deps{Core: sdeps.Core{
		Config:  &config.Config{DataDir: dir, DBPath: core},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "9.9.9",
	}}}

	rec := httptest.NewRecorder()
	h.Backup(rec, httptest.NewRequest(http.MethodGet, "/admin/api/system/backup", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment; filename=") || !strings.Contains(cd, "openfabrica-backup-") {
		t.Errorf("Content-Disposition = %q, want an attachment named openfabrica-backup-*", cd)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("response is not a valid zip: %v", err)
	}
	have := map[string]bool{}
	for _, f := range zr.File {
		have[f.Name] = true
	}
	for _, want := range []string{"manifest.json", "openfabrica.db", "projects/3/project.db"} {
		if !have[want] {
			t.Errorf("archive is missing %s (has %v)", want, have)
		}
	}
}

// The effective-config view reports the operational settings and names the CSP
// policy, but never carries the signing or encryption key.
func TestEffectiveConfigOmitsSecrets(t *testing.T) {
	h := &SystemHandler{deps: &Deps{Core: sdeps.Core{Config: &config.Config{
		AdminPort: 5734, AdminBindAddr: "0.0.0.0", PublicPort: 5733, DataDir: "/srv/of",
		JWTSecret: "jwt-secret-value-should-not-leak", EncryptionKey: "enc-key-value-should-not-leak",
		PublicCSP: config.HardenedPublicCSP, RateLimitRate: 50, RateLimitBurst: 100,
	}}}}

	rec := httptest.NewRecorder()
	h.Config(rec, httptest.NewRequest(http.MethodGet, "/admin/api/system/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"jwt-secret-value-should-not-leak", "enc-key-value-should-not-leak", "jwt_secret", "encryption_key"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
	}
	for _, want := range []string{`"admin_exposed":true`, `"public_csp":"hardened"`, `"data_dir":"/srv/of"`, `"cors_origins":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("response lacks %s: %s", want, body)
		}
	}
}
