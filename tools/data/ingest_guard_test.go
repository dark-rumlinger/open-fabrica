package data

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIngestPathBlocked(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	docs := filepath.Join(root, "docs")
	for _, d := range []string{dataDir, docs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(dataDir, "encryption.key")
	_ = os.WriteFile(key, []byte("k"), 0o600)
	note := filepath.Join(docs, "note.txt")
	_ = os.WriteFile(note, []byte("hello"), 0o600)
	cfg := filepath.Join(docs, "config.json")
	_ = os.WriteFile(cfg, []byte("{}"), 0o600)

	if ingestPathBlocked(dataDir, key) == "" || ingestPathBlocked(dataDir, dataDir) == "" {
		t.Error("data directory must be blocked")
	}
	if ingestPathBlocked(dataDir, cfg) == "" {
		t.Error("config.json must be blocked")
	}
	if msg := ingestPathBlocked(dataDir, note); msg != "" {
		t.Errorf("ordinary document blocked: %s", msg)
	}
}
