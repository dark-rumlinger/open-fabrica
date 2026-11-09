package config

import (
	"strings"
	"testing"
)

func TestExpandCSPPreset(t *testing.T) {
	if got := ExpandCSPPreset(CSPPresetHardened); got != HardenedPublicCSP {
		t.Errorf("hardened keyword did not expand: got %q", got)
	}
	if got := ExpandCSPPreset("  hardened  "); got != HardenedPublicCSP {
		t.Errorf("padded hardened keyword did not expand: got %q", got)
	}
	const custom = "default-src 'self'; script-src 'self'"
	if got := ExpandCSPPreset(custom); got != custom {
		t.Errorf("literal policy must pass through unchanged: got %q", got)
	}
	if got := ExpandCSPPreset(""); got != "" {
		t.Errorf("empty must pass through unchanged: got %q", got)
	}
}

// The hardened preset only earns its name if it pins script-src to self+nonce -
// that's what drops the script 'unsafe-inline' the permissive default leans on.
// 'wasm-unsafe-eval' rides along to keep WASM compiling but grants no JS eval().
func TestHardenedPublicCSP_LocksScripts(t *testing.T) {
	if !strings.Contains(HardenedPublicCSP, "script-src 'self' 'wasm-unsafe-eval' {nonce};") {
		t.Errorf("hardened policy must pin script-src to self+wasm+nonce: %q", HardenedPublicCSP)
	}
	if strings.Contains(HardenedPublicCSP, "'unsafe-eval'") {
		t.Errorf("hardened policy must not grant broad 'unsafe-eval': %q", HardenedPublicCSP)
	}
}

func TestPermissiveDefaultIssues(t *testing.T) {
	if got := PermissiveDefaultIssues(false, nil, DefaultPublicCSP); got != nil {
		t.Errorf("local-only instance warned: %v", got)
	}
	if got := PermissiveDefaultIssues(true, nil, DefaultPublicCSP); len(got) != 2 {
		t.Errorf("public + open defaults = %v, want 2 issues", got)
	}
	if got := PermissiveDefaultIssues(true, []string{"https://app.example.com"}, "default-src 'self'"); got != nil {
		t.Errorf("tightened instance warned: %v", got)
	}
}
