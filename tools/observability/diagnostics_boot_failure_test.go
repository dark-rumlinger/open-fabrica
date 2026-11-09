package observability

import (
	"testing"

	"github.com/chromedp/cdproto/runtime"
)

// A boot/subsystem failure the page CAUGHT and logged (the swallow-and-warn
// pattern that ships a feature dead) never throws, so the exception probe stays
// silent - but a /files/-originated logged Error must still hard-fail the render.
func TestFailureReason_BootFailureIsHardFail(t *testing.T) {
	r := &pageRenderReport{bootFailures: []string{
		"subsystem load failed ./universe.hud.js ReferenceError: register is not defined",
	}}
	if reason, failed := r.failureReason(); !failed {
		t.Fatalf("a project-code boot failure must hard-fail, got reason=%q failed=%v", reason, failed)
	}
}

// A library console.error (surfaced, not classified as a boot failure) stays
// informational so the model judges it - the platform must not auto-fail on it,
// preserving the existing no-false-positive philosophy for library noise.
func TestFailureReason_LibraryConsoleErrorNotFatal(t *testing.T) {
	r := &pageRenderReport{consoleErrors: []string{
		"THREE.Object3D.add: object not an instance of THREE.Object3D",
	}}
	if _, failed := r.failureReason(); failed {
		t.Fatalf("a surfaced library console.error must not hard-fail the render")
	}
}

// The classifier that separates generated project code (a /files/ frame) from
// library/CDN noise - the linchpin that keeps the boot-failure promotion precise.
func TestConsoleFromProjectCode(t *testing.T) {
	project := &runtime.EventConsoleAPICalled{
		StackTrace: &runtime.StackTrace{CallFrames: []*runtime.CallFrame{
			{URL: "http://app.local:8080/files/universe.js"},
		}},
	}
	if !consoleFromProjectCode(project, "subsystem load failed") {
		t.Errorf("a /files/ stack frame must classify as project code")
	}

	library := &runtime.EventConsoleAPICalled{
		StackTrace: &runtime.StackTrace{CallFrames: []*runtime.CallFrame{
			{URL: "https://esm.sh/three@0.160.0/build/three.module.js"},
		}},
	}
	if consoleFromProjectCode(library, "THREE warning") {
		t.Errorf("a CDN/library frame must not classify as project code")
	}

	// Fallback: the serialized Error stack carries the /files/ URL even when the
	// console call frame itself does not.
	noStack := &runtime.EventConsoleAPICalled{}
	if !consoleFromProjectCode(noStack, "ReferenceError at http://x/files/universe.hud.js:568") {
		t.Errorf("a /files/ URL in the message must classify as project code")
	}
	if consoleFromProjectCode(noStack, "just a plain warning, no origin") {
		t.Errorf("no /files/ anywhere must not classify as project code")
	}
}
