package rotatelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterRotatesAndKeepsABoundedNumberOfFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "app.log")
	w, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	line := strings.Repeat("x", 40) + "\n" // 41 bytes: two fit, the third rotates
	for i := 0; i < 12; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"app.log", "app.log.1", "app.log.2"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
			t.Errorf("%s should exist: %v", name, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("only 2 rotated files are kept")
	}
	info, _ := os.Stat(path)
	if info.Size() > 100 {
		t.Errorf("active file is %d bytes, over the 100 cap", info.Size())
	}
}

func TestWriterAppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(path, []byte("earlier\n"), 0o600)
	w, err := Open(path, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("later\n"))
	w.Close()
	b, _ := os.ReadFile(path)
	if string(b) != "earlier\nlater\n" {
		t.Errorf("file = %q", b)
	}
}
