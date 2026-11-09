package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/security"
)

// Each way provider resolution can fail on configuration must pause the worker
// at once (isPermanentError) instead of spending three stage retries. This used
// to hinge on substring-matching the error text, so rewording a message quietly
// turned a pause into a retry storm; it is now a typed error, pinned here per
// failure path through the real resolver.
func TestProviderConfigErrorsArePermanent(t *testing.T) {
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		seed  []string
		wantS string
	}{
		{name: "no model configured", wantS: "no model configured"},
		{
			name: "provider missing its API key",
			seed: []string{
				`INSERT INTO providers (id, name, vendor, transport, base_url, requires_api_key) VALUES (1, 'Keyed', 'keyed', 'openai-chat', 'http://127.0.0.1:0/v1', 1)`,
				`INSERT INTO llm_models (id, provider_id, model_id, display_name) VALUES (1, 1, 'm', 'M')`,
				`INSERT INTO projects (id, name, published, mode, llm_model_id) VALUES (1, 'P', 1, 'building', 1)`,
			},
			wantS: "has no API key",
		},
		{
			name: "undecryptable API key",
			seed: []string{
				`INSERT INTO providers (id, name, vendor, transport, base_url, requires_api_key, api_key_encrypted) VALUES (1, 'Keyed', 'keyed', 'openai-chat', 'http://127.0.0.1:0/v1', 1, 'not-ciphertext')`,
				`INSERT INTO llm_models (id, provider_id, model_id, display_name) VALUES (1, 1, 'm', 'M')`,
				`INSERT INTO projects (id, name, published, mode, llm_model_id) VALUES (1, 'P', 1, 'building', 1)`,
			},
			wantS: "failed to decrypt",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gdb, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer gdb.Close()
			for _, s := range c.seed {
				if _, err := gdb.ExecWrite(s); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			r := newProviderResolver(&Deps{DB: gdb, Encryptor: enc}, 1)
			_, err = r.resolve()
			if err == nil {
				t.Fatal("resolve succeeded; want a configuration error")
			}
			if !strings.Contains(err.Error(), c.wantS) {
				t.Fatalf("error = %q, want it to mention %q (owners read this text)", err, c.wantS)
			}
			if !errors.Is(err, ErrProviderConfig) || !isPermanentError(err) {
				t.Fatalf("%v: want ErrProviderConfig and permanent", err)
			}
			// Still permanent after the stage wraps it on the way to the breaker.
			if wrapped := fmt.Errorf("plan stage: %w", err); !isPermanentError(wrapped) {
				t.Fatal("a wrapped configuration error lost its permanence")
			}
		})
	}
}

// Text alone no longer decides: an unrelated error that happens to say
// "no api key" is retried like any other transient failure.
func TestPermanenceIsTypedNotTextual(t *testing.T) {
	if isPermanentError(errors.New("upstream said: no api key header echoed")) {
		t.Fatal("an untyped error must not be treated as a configuration failure by its text")
	}
}
