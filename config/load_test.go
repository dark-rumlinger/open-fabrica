package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadIn runs LoadFrom against a throwaway data dir with a clean environment.
func loadIn(t *testing.T, configPath string, env map[string]string) (*Config, error) {
	t.Helper()
	for _, k := range []string{
		"OPENFABRICA_ADMIN_PORT", "OPENFABRICA_PUBLIC_PORT", "OPENFABRICA_LLM_TIMEOUT",
		"OPENFABRICA_NO_BROWSER", "OPENFABRICA_TLS_DISABLED", "OPENFABRICA_JWT_SECRET",
		"OPENFABRICA_ENCRYPTION_KEY", "OPENFABRICA_ADMIN_BIND", "OPENFABRICA_ADMIN_EXPOSE",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("OPENFABRICA_DATA_DIR", t.TempDir())
	for k, v := range env {
		t.Setenv(k, v)
	}
	return LoadFrom(configPath)
}

func TestLoadFromMissingFileIsFine(t *testing.T) {
	if _, err := loadIn(t, filepath.Join(t.TempDir(), "absent.json"), nil); err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
}

func TestLoadFromUnreadableFileIsAnError(t *testing.T) {
	// A directory where the file should be: it exists but cannot be read.
	_, err := loadIn(t, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("an unreadable config path must fail loudly, got %v", err)
	}
}

func TestBooleanEnvValues(t *testing.T) {
	for v, wantOff := range map[string]bool{"1": true, "true": true, "TRUE": true, "yes": true, "0": false, "false": false, "no": false} {
		cfg, err := loadIn(t, filepath.Join(t.TempDir(), "x.json"), map[string]string{"OPENFABRICA_NO_BROWSER": v})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AutoOpenBrowser == wantOff {
			t.Errorf("OPENFABRICA_NO_BROWSER=%q: AutoOpenBrowser=%v", v, cfg.AutoOpenBrowser)
		}
	}
}

func TestInvalidNumericEnvKeepsDefault(t *testing.T) {
	cfg, err := loadIn(t, filepath.Join(t.TempDir(), "x.json"), map[string]string{"OPENFABRICA_LLM_TIMEOUT": "ten minutes", "OPENFABRICA_PUBLIC_PORT": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMTimeoutSec != 0 || cfg.PublicPort != DefaultPublicPort {
		t.Errorf("a typo overrode a default: timeout=%d port=%d", cfg.LLMTimeoutSec, cfg.PublicPort)
	}
}

func TestKeyFileTrailingNewlineIsAccepted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENFABRICA_DATA_DIR", dir)
	for _, k := range []string{"OPENFABRICA_JWT_SECRET", "OPENFABRICA_ENCRYPTION_KEY", "OPENFABRICA_ADMIN_BIND"} {
		t.Setenv(k, "")
	}
	first, err := LoadFrom(filepath.Join(t.TempDir(), "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, ".security", "encryption.key")
	if err := os.WriteFile(keyPath, []byte(first.EncryptionKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := LoadFrom(filepath.Join(t.TempDir(), "x.json"))
	if err != nil {
		t.Fatalf("a key file ending in a newline must load: %v", err)
	}
	if second.EncryptionKey != first.EncryptionKey {
		t.Error("the trimmed key differs from the original")
	}
}
