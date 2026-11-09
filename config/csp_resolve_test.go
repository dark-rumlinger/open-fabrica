package config

import "testing"

// ResolveCSP is the single home for "which policy does this project actually serve
// under". Both the public router's response header and project_state's snapshot read
// it, so these cases pin the precedence once for both.
func TestResolveCSP(t *testing.T) {
	cases := []struct {
		name     string
		override string
		instance string
		want     string
	}{
		{"no override falls back to the instance default", "", DefaultPublicCSP, DefaultPublicCSP},
		{"override wins over the instance default", HardenedPublicCSP, DefaultPublicCSP, HardenedPublicCSP},
		{"override preset expands", CSPPresetHardened, DefaultPublicCSP, HardenedPublicCSP},
		{"instance preset expands", "", CSPPresetHardened, HardenedPublicCSP},
		{"whitespace-only override is no override", "   ", DefaultPublicCSP, DefaultPublicCSP},
		{"a literal policy passes through", "", "default-src 'self'", "default-src 'self'"},
		// The documented OPENFABRICA_PUBLIC_CSP="" opt-out means "send no header".
		// Substituting DefaultPublicCSP here would make every reader report a policy
		// the server never sends, which is exactly how the mirror this replaced went
		// silently wrong.
		{"empty instance default stays empty", "", "", ""},
		{"an override still applies over an empty instance default", CSPPresetHardened, "", HardenedPublicCSP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveCSP(tc.override, tc.instance); got != tc.want {
				t.Errorf("ResolveCSP(%q, %q) = %q, want %q", tc.override, tc.instance, got, tc.want)
			}
		})
	}
}

// CSPScriptSources owns the script-src / default-src fallback precedence so no
// caller re-implements it. The hardened preset is the case that matters: it sets
// BOTH, and only script-src governs scripts.
func TestCSPScriptSources(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		want   string
	}{
		{"script-src wins over default-src", HardenedPublicCSP, "'self' 'wasm-unsafe-eval' {nonce}"},
		{"falls back to default-src", DefaultPublicCSP, "* data: blob: 'unsafe-inline' 'wasm-unsafe-eval'"},
		{"neither set means unconstrained", "object-src 'none'", ""},
		{"empty policy means unconstrained", "", ""},
		{"directive name match is case-insensitive", "Script-Src 'self'", "'self'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CSPScriptSources(tc.policy); got != tc.want {
				t.Errorf("CSPScriptSources(%q) = %q, want %q", tc.policy, got, tc.want)
			}
		})
	}
}
