// Command encgen regenerates knowledge/encrypted/ from the plaintext knowledge
// tree. The plaintext files (mental-model.md, guides/**, stages/**, contracts/**)
// stay on disk as the thing developers edit; this tool mirrors them into
// encrypted/ with identical relative paths and filenames, encrypting only the
// file contents. embed.go embeds encrypted/, so the binary never carries the
// cleartext prompts.
//
// Run from the knowledge/ directory (that is what `go generate ./knowledge`
// does): `go run ./encgen`. Output is deterministic, so a re-run with unchanged
// sources rewrites nothing.
package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"openfabrica/internal/mdcrypt"
	"openfabrica/internal/repopath"
)

// encRoot is the output directory and the exact name embed.go embeds.
const encRoot = "encrypted"

// plaintextRoots are the knowledge sources, matching the old go:embed list in
// embed.go. A plain file (mental-model.md) and directories are both handled.
var plaintextRoots = []string{"mental-model.md", "guides", "stages", "contracts"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "encgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Resolve paths against the repo's knowledge/ dir regardless of the caller's
	// working directory, so this behaves the same under `go generate ./knowledge`
	// and a `go run openfabrica/knowledge/encgen` from the repo root.
	repoRoot, err := repopath.Root()
	if err != nil {
		return err
	}
	if err := os.Chdir(filepath.Join(repoRoot, "knowledge")); err != nil {
		return err
	}

	want := map[string]bool{} // encrypted paths this run should produce (slash form)
	for _, root := range plaintextRoots {
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("stat %s (run from the knowledge/ directory): %w", root, err)
		}
		if !info.IsDir() {
			if err := encryptFile(root, want); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			return encryptFile(path, want)
		})
		if err != nil {
			return err
		}
	}
	return prune(want)
}

// encryptFile encrypts src (a plaintext path relative to knowledge/) into
// encRoot/src, creating parent dirs. It writes only when the content changed so
// re-runs leave untouched files alone.
func encryptFile(src string, want map[string]bool) error {
	plaintext, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	// filepath.ToSlash keeps the nonce-deriving name stable across OSes, so the
	// output is identical whether generated on Windows or Linux.
	blob, err := mdcrypt.Encrypt(filepath.ToSlash(src), plaintext)
	if err != nil {
		return fmt.Errorf("encrypting %s: %w", src, err)
	}
	out := filepath.Join(encRoot, src)
	want[filepath.ToSlash(out)] = true
	if existing, err := os.ReadFile(out); err == nil && bytes.Equal(existing, blob) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	return nil
}

// prune removes any file under encRoot that this run did not produce, so a
// renamed or deleted plaintext file does not leave a stale ciphertext behind
// (which would surface in the binary and fail the drift test).
func prune(want map[string]bool) error {
	if _, err := os.Stat(encRoot); os.IsNotExist(err) {
		return nil
	}
	var stale []string
	err := filepath.WalkDir(encRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !want[filepath.ToSlash(path)] {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(stale)
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("removing stale %s: %w", p, err)
		}
	}
	return nil
}
