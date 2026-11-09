package snapshotbuild

import (
	"strings"
	"testing"

	"openfabrica/config"
)

func renderCSP(t *testing.T, policy string) string {
	t.Helper()
	var b strings.Builder
	writeCSP(&b, withExplainers, policy)
	return b.String()
}

// The permissive default constrains nothing the model would otherwise get wrong, and
// project_state rides EVERY build turn - so it must cost zero bytes there. This is the
// whole reason the fact lives in the snapshot instead of behind a tool call.
func TestCSPSectionSilentOnTheDefault(t *testing.T) {
	for name, policy := range map[string]string{
		"shipped default":       config.DefaultPublicCSP,
		"no policy at all":      "",
		"scripts unconstrained": "object-src 'none'; frame-ancestors 'self'",
	} {
		if got := renderCSP(t, policy); got != "" {
			t.Errorf("%s must render nothing, got:\n%s", name, got)
		}
	}
}

// Under the hardened preset the model must learn the two things that actually change
// what it may write. Inline <script> is NOT one of them: the renderer nonce-stamps
// every inline script, so saying it is blocked would be false.
func TestCSPSectionNamesWhatHardenedBlocks(t *testing.T) {
	out := renderCSP(t, config.HardenedPublicCSP)
	if out == "" {
		t.Fatal("the hardened preset must be reported - it changes what the build may write")
	}
	for _, want := range []string{
		"### Content-Security-Policy",
		"on*=",
		"addEventListener",
		"cross-origin scripts are blocked",
		"'self' 'wasm-unsafe-eval' {nonce}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hardened render should mention %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "inline `<script>` is blocked") || strings.Contains(out, "inline script is blocked") {
		t.Errorf("inline <script> keeps running under a nonce policy; the render must not claim otherwise:\n%s", out)
	}
}

// script-src governs when present, even though the hardened preset's default-src
// carries 'unsafe-inline'. Reading the wrong directive would report the exact
// opposite of what the browser enforces.
func TestCSPSectionReadsScriptSrcNotDefaultSrc(t *testing.T) {
	out := renderCSP(t, "default-src * 'unsafe-inline'; script-src 'self'")
	if !strings.Contains(out, "on*=") {
		t.Errorf("script-src without 'unsafe-inline' blocks inline handlers regardless of default-src:\n%s", out)
	}
}

// A policy that tightens something OTHER than scripts changes nothing about what the
// model may write, so it stays silent rather than spending bytes on a non-event.
func TestCSPSectionSilentWhenScriptsStayOpen(t *testing.T) {
	if got := renderCSP(t, "default-src * data: blob: 'unsafe-inline' https:; object-src 'none'"); got != "" {
		t.Errorf("a policy that leaves scripts open must render nothing, got:\n%s", got)
	}
}

// Through the real Render() path, not just the helper: a section that renders correctly
// in isolation but is never called, or is called from the wrong path, ships nothing.
func TestCSPSectionReachesTheRenderedSnapshot(t *testing.T) {
	snap := &ProjectSnapshot{
		Pages:        []PageRef{{Path: "/", Title: "Home"}},
		EffectiveCSP: config.HardenedPublicCSP,
	}
	out := snap.Render()
	if !strings.Contains(out, "### Content-Security-Policy") {
		t.Fatalf("the section must reach the rendered snapshot:\n%s", out)
	}

	snap.EffectiveCSP = config.DefaultPublicCSP
	if strings.Contains(snap.Render(), "Content-Security-Policy") {
		t.Error("the permissive default must add nothing to the snapshot the model reads every turn")
	}
}

// The policy is a standing environment fact that arrives with the baseline, not a
// per-write change. A delta that re-shipped it would repeat the whole block on every
// write turn - the exact cost the dataOnly delta render exists to avoid.
func TestCSPSectionDoesNotRepeatOnEveryDelta(t *testing.T) {
	snap := &ProjectSnapshot{
		Pages:        []PageRef{{Path: "/", Title: "Home"}},
		EffectiveCSP: config.HardenedPublicCSP,
	}
	delta := snap.RenderDelta(map[string]string{}, nil)
	if strings.Contains(delta, "Content-Security-Policy") {
		t.Errorf("a change delta must not restate the standing policy:\n%s", delta)
	}
}

// The owner's choice to turn user-flow tests off reaches the model through the rendered
// snapshot, and the default (on) adds nothing.
func TestUserFlowsOffSectionReachesTheRenderedSnapshot(t *testing.T) {
	snap := &ProjectSnapshot{Pages: []PageRef{{Path: "/", Title: "Home"}}, UserFlowsOff: true}
	if out := snap.Render(); !strings.Contains(out, "### User-flow tests") || !strings.Contains(out, "OFF") {
		t.Fatalf("the off state must be stated in the snapshot:\n%s", out)
	}
	snap.UserFlowsOff = false
	if strings.Contains(snap.Render(), "User-flow tests") {
		t.Error("the default must add nothing to the snapshot the model reads every turn")
	}
}
