package config

import (
	"strings"

	"openfabrica/internal/msgcat"
)

const (
	DefaultAdminPort  = 5734 // Admin panel HTTP server port
	DefaultPublicPort = 5733 // Public-facing project HTTP server port

	// DefaultAdminBindAddr keeps the admin/control plane on loopback so a host
	// that exposes the binary to a network does not also hand the admin panel
	// (which can read every project's data and secrets) to that network. Binding
	// to a routable address requires an explicit opt-in (OPENFABRICA_ADMIN_EXPOSE=1).
	DefaultAdminBindAddr  = "127.0.0.1"
	DefaultDataDir        = "./data"
	DefaultLogLevel       = "info"
	DefaultSeedPath       = "./seed.json"
	DefaultRateLimitRate  = 100 // Requests per second per IP (public endpoints)
	DefaultRateLimitBurst = 200 // Burst capacity per IP (public endpoints)
	DefaultLLMTimeoutSec  = 900 // 15 minutes; non-streaming providers

	// DefaultEngineDailyTokenBudget caps engine token spend per project per UTC
	// day: the build loop runs unattended on the operator's paid key, so an
	// unlimited default would let a runaway loop bill without bound. 0 disables
	// the cap (explicit opt-in; the setup wizard stores 0 for free local providers).
	DefaultEngineDailyTokenBudget = 5_000_000

	// DefaultPublicCSP is a permissive baseline for HTML on the public project
	// router: it blocks plugin objects, locks base href, and restricts framing,
	// but leaves script/style sources open. It carries 'wasm-unsafe-eval' so
	// WebAssembly modules (TensorFlow.js, MediaPipe, ffmpeg.wasm, game engines)
	// compile - the browser gates WASM behind CSP and a wildcard host source does
	// not imply it. It deliberately omits the broader 'unsafe-eval': generated
	// apps never need JavaScript eval(), so withholding it removes a DOM-XSS
	// escalation at no cost, while 'wasm-unsafe-eval' permits only WASM compilation.
	// The literal token {nonce} in a policy is replaced per response. See
	// server/middleware/csp.go.
	DefaultPublicCSP = "default-src * data: blob: 'unsafe-inline' 'wasm-unsafe-eval'; object-src 'none'; base-uri 'self'; frame-ancestors 'self'"
)

// HardenedPublicCSP is the opt-in strict policy selected by the "hardened"
// keyword. It locks scripts to 'self' plus a per-request nonce, dropping script
// 'unsafe-inline' (the main stored-XSS vector); app-authored inline handlers and
// third-party <script src> stop running - the accepted cost. It keeps
// 'wasm-unsafe-eval' in script-src so 'self' WASM modules still compile (WASM is
// gated by script-src once that directive is present); this permits only WASM
// compilation, not JavaScript eval().
const HardenedPublicCSP = "default-src * data: blob: 'unsafe-inline'; script-src 'self' 'wasm-unsafe-eval' {nonce}; object-src 'none'; base-uri 'self'; frame-ancestors 'self'"

// CSPPresetHardened is the keyword ExpandCSPPreset maps to HardenedPublicCSP.
const CSPPresetHardened = "hardened"

// ExpandCSPPreset maps a CSP preset keyword to its policy string so an operator
// can opt into a hardened policy without hand-writing one. Any other value - a
// literal policy, or "" - passes through unchanged.
func ExpandCSPPreset(raw string) string {
	if strings.TrimSpace(raw) == CSPPresetHardened {
		return HardenedPublicCSP
	}
	return raw
}

// ResolveCSP returns the Content-Security-Policy a project actually serves under:
// its own override when set, otherwise the instance default, each expanded through
// ExpandCSPPreset. An empty result means "send no CSP header at all", which is the
// documented OPENFABRICA_PUBLIC_CSP="" opt-out and must survive as itself - a caller
// that substitutes DefaultPublicCSP here would report a policy the server never sends.
//
// This is the ONE home for that precedence. Every surface that needs to know the
// effective policy - the public router's response header, project_state's snapshot -
// calls this rather than re-deriving it, so a second copy cannot drift from the one
// the browser is actually handed.
func ResolveCSP(projectOverride, instanceDefault string) string {
	if p := strings.TrimSpace(projectOverride); p != "" {
		return ExpandCSPPreset(p)
	}
	return ExpandCSPPreset(instanceDefault)
}

// CSPScriptSources returns the source list that governs <script> loading under
// policy: script-src when the policy sets it, else default-src, else "" when the
// policy constrains scripts not at all. Callers read the returned list to answer
// "does this policy permit X" without each re-implementing the fallback precedence.
func CSPScriptSources(policy string) string {
	if d := cspDirective(policy, "script-src"); d != "" {
		return d
	}
	return cspDirective(policy, "default-src")
}

// cspDirective returns the source list for name, or "" when policy does not set it.
func cspDirective(policy, name string) string {
	for _, part := range strings.Split(policy, ";") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) > 0 && strings.EqualFold(fields[0], name) {
			return strings.Join(fields[1:], " ")
		}
	}
	return ""
}

// PermissiveDefaultIssues lists the wide-open public defaults that matter once
// the instance is internet-reachable: a wildcard CORS policy (empty origins) and
// the permissive built-in CSP. It returns nil for a local-only instance (no
// domains) or when the defaults have been tightened. Pure so the startup warning
// and the dashboard's live status share one decision and it is unit-testable.
func PermissiveDefaultIssues(hasActiveDomains bool, corsOrigins []string, publicCSP string) []string {
	if !hasActiveDomains {
		return nil
	}
	var issues []string
	if len(corsOrigins) == 0 {
		issues = append(issues, msgcat.Mark("CORS allows all origins (cors_origins is empty)"))
	}
	if publicCSP == DefaultPublicCSP {
		issues = append(issues, msgcat.Mark("public CSP is the permissive default"))
	}
	return issues
}
