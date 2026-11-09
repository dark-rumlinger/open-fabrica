package backup

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func mkDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func queryString(t *testing.T, path, q string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s string
	if err := db.QueryRow(q).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func newInstance(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mkDB(t, filepath.Join(dir, "openfabrica.db"), `CREATE TABLE settings (k TEXT, v TEXT)`, `INSERT INTO settings VALUES ('theme','dark')`)
	pdir := filepath.Join(dir, "projects", "7")
	mkDB(t, filepath.Join(pdir, "project.db"),
		`CREATE TABLE of_files (id INTEGER PRIMARY KEY, storage TEXT, disk_path TEXT)`,
		`INSERT INTO of_files (storage, disk_path) VALUES ('disk', '`+filepath.ToSlash(filepath.Join(pdir, "files", "logo.png"))+`')`)
	write(t, filepath.Join(pdir, "files", "logo.png"), "PNG-BYTES")
	write(t, filepath.Join(pdir, "project.db-wal"), "stale")
	write(t, filepath.Join(dir, ".security", "encryption.key"), strings.Repeat("ab", 32))
	write(t, filepath.Join(dir, "certs", "acme.json"), "{}")
	write(t, filepath.Join(dir, "backups", "old.zip"), "previous backup")
	return dir
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	src := newInstance(t)
	var buf bytes.Buffer
	m, err := Create(&buf, Options{DataDir: src, Version: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(m.Projects) != 1 || m.Projects[0] != 7 {
		t.Fatalf("manifest projects = %v", m.Projects)
	}

	// Restore into a different directory, as after moving to a new machine.
	dst := filepath.Join(t.TempDir(), "newhome")
	got, err := Restore(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{DataDir: dst}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got.Version != "test" {
		t.Errorf("version = %q", got.Version)
	}
	if v := queryString(t, filepath.Join(dst, "openfabrica.db"), `SELECT v FROM settings WHERE k='theme'`); v != "dark" {
		t.Errorf("core db content = %q", v)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "projects", "7", "files", "logo.png")); err != nil || string(b) != "PNG-BYTES" {
		t.Errorf("project file: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, ".security", "encryption.key")); string(b) != strings.Repeat("ab", 32) {
		t.Error("key file not restored")
	}
	if _, err := os.Stat(filepath.Join(dst, "certs", "acme.json")); err != nil {
		t.Error("certs not restored")
	}
	if _, err := os.Stat(filepath.Join(dst, "backups")); err == nil {
		t.Error("earlier backups must not be nested into a new one")
	}
	if _, err := os.Stat(filepath.Join(dst, "projects", "7", "project.db-wal")); err == nil {
		t.Error("a stale WAL sidecar was carried over")
	}
	// The stored path now points into the restored location, not the old machine.
	relinked := queryString(t, filepath.Join(dst, "projects", "7", "project.db"), `SELECT disk_path FROM of_files`)
	if !strings.HasPrefix(filepath.ToSlash(relinked), filepath.ToSlash(dst)) {
		t.Errorf("disk_path %q was not re-anchored under %q", relinked, dst)
	}
}

func TestRestoreRefusesToReplaceALiveInstance(t *testing.T) {
	src := newInstance(t)
	var buf bytes.Buffer
	if _, err := Create(&buf, Options{DataDir: src}); err != nil {
		t.Fatal(err)
	}
	other := newInstance(t)
	if _, err := Restore(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{DataDir: other}, false); err == nil {
		t.Fatal("restore over an existing instance must need --force")
	}
	if _, err := Restore(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{DataDir: other}, true); err != nil {
		t.Fatalf("forced restore: %v", err)
	}
}

func TestRestoreRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(manifestName)
	mw.Write([]byte(`{"format":1,"projects":[]}`))
	ew, _ := zw.Create("../escape.txt")
	ew.Write([]byte("x"))
	zw.Close()

	parent := t.TempDir()
	dst := filepath.Join(parent, "data")
	if _, err := Restore(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{DataDir: dst}, false); err == nil {
		t.Fatal("a ../ entry was accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("a file was written outside the data directory")
	}
}

func TestRestoreRejectsForeignArchive(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("something.txt")
	w.Write([]byte("x"))
	zw.Close()
	if _, err := Restore(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{DataDir: t.TempDir()}, false); err == nil {
		t.Fatal("an archive without a manifest was accepted")
	}
}

// A forced restore of a bad archive must fail before touching the live
// instance: the replaced data survives and no staging directory is left behind.
func TestForcedRestoreOfBadArchiveLeavesInstanceIntact(t *testing.T) {
	live := newInstance(t)
	before := queryString(t, filepath.Join(live, "projects", "7", "project.db"), `SELECT name FROM sqlite_master WHERE type='table' LIMIT 1`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create(manifestName)
	mw.Write([]byte(`{"format":1,"projects":[]}`))
	cw, _ := zw.Create("certs/acme.json")
	cw.Write([]byte("new"))
	ew, _ := zw.Create("../escape.txt")
	ew.Write([]byte("x"))
	zw.Close()

	if _, err := Restore(bytes.NewReader(buf.Bytes()), int64(buf.Len()), Options{DataDir: live}, true); err == nil {
		t.Fatal("a ../ entry was accepted")
	}
	after := queryString(t, filepath.Join(live, "projects", "7", "project.db"), `SELECT name FROM sqlite_master WHERE type='table' LIMIT 1`)
	if after != before {
		t.Errorf("live project database changed: %q -> %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(live, restoreStagingName)); err == nil {
		t.Error("staging directory was left behind")
	}
	if b, _ := os.ReadFile(filepath.Join(live, "certs", "acme.json")); string(b) == "new" {
		t.Error("a staged file leaked into the live instance")
	}
}
