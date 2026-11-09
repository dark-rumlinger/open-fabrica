package observability

import (
	"strings"
	"testing"
)

func TestModuleReferencesDOM(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"canvas engine", "var c = document.getElementById('game-canvas');", true},
		{"getContext", "ctx = canvasEl.getContext('2d');", true},
		{"raf loop", "requestAnimationFrame(tick);", true},
		{"event wiring", "window.addEventListener('keydown', onKey);", true},
		{"pure logic", "export function add(a, b) { return a + b; }", false},
		{"window global publish only", "(function(){ window.Scoring = { score: function(n){ return n*100; } }; })();", false},
	}
	for _, c := range cases {
		if got := moduleReferencesDOM(c.body); got != c.want {
			t.Errorf("%s: moduleReferencesDOM=%v want %v", c.name, got, c.want)
		}
	}
}

// A page that painted visible NaN/undefined text (a math/data bug that throws
// nothing, e.g. a 3D HUD whose NaN camera blacked the scene) must surface an
// advisory naming the offending text - never pass silently.
func TestJunkTextWarning(t *testing.T) {
	if w := (&pageRenderReport{}).junkTextWarning(); w != "" {
		t.Errorf("a clean report must not warn, got %q", w)
	}
	r := &pageRenderReport{junkText: []string{"COORD NaN,NaN,NaN", "VEL NaN u/s"}}
	w := r.junkTextWarning()
	if w == "" {
		t.Fatal("a visible NaN leak must produce an advisory")
	}
	if !strings.Contains(w, "COORD NaN,NaN,NaN") {
		t.Errorf("advisory must quote the offending text, got %q", w)
	}
}

// A canvas-app page whose render loop never ran (a full-viewport <canvas> mounted
// but no rAF frame, e.g. a boot stalled waiting on a socket that never connected)
// must surface an advisory - it is the exact false confidence visible_chars gives
// when only a loading overlay painted. A page with a running loop, or a non-canvas
// text page, must stay silent.
func TestStuckBootWarning(t *testing.T) {
	cases := []struct {
		name string
		r    pageRenderReport
		warn bool
	}{
		{"stalled canvas boot", pageRenderReport{canvasPresent: true, canvasDominant: true, canvasSized: true, rafFrames: 0}, true},
		{"one stray frame still stalled", pageRenderReport{canvasDominant: true, rafFrames: 1}, true},
		{"running render loop", pageRenderReport{canvasPresent: true, canvasDominant: true, canvasSized: true, rafFrames: 240}, false},
		{"canvas present but not dominant", pageRenderReport{canvasPresent: true, canvasDominant: false, rafFrames: 0}, false},
		{"plain text page (no canvas, no rAF)", pageRenderReport{rafFrames: 0}, false},
	}
	for _, c := range cases {
		got := c.r.stuckBootWarning() != ""
		if got != c.warn {
			t.Errorf("%s: stuckBootWarning present=%v want %v", c.name, got, c.warn)
		}
	}
	// The advisory must name the render-loop signal so the agent knows what to check.
	w := (&pageRenderReport{canvasDominant: true, rafFrames: 0}).stuckBootWarning()
	if !strings.Contains(w, "rafFrames=0") || !strings.Contains(w, "loop never started") {
		t.Errorf("advisory must explain the render-loop signal, got %q", w)
	}
}
