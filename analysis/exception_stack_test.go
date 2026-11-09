package analysis

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/runtime"
)

func frame(url, fn string, line int64) *runtime.CallFrame {
	return &runtime.CallFrame{URL: url, FunctionName: fn, LineNumber: line}
}

// TestFirstProjectFrameVendorOnly pins the no-project-frame case: a throw whose
// whole stack is vendor-internal must yield "" and the model-facing message
// must say so explicitly, redirecting attention from the library file.
func TestFirstProjectFrameVendorOnly(t *testing.T) {
	ed := &runtime.ExceptionDetails{
		Text: "Uncaught",
		Exception: &runtime.RemoteObject{
			Description: "TypeError: Cannot read properties of null (reading 'trim')\n    at onFirstUse (https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js:20276:54)\n    at WebGLProgram.getUniforms (https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js:20291:12)",
		},
		StackTrace: &runtime.StackTrace{CallFrames: []*runtime.CallFrame{
			frame("https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js", "onFirstUse", 20275),
			frame("https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js", "getUniforms", 20290),
		}},
	}
	if got := FirstProjectFrame(ed); got != "" {
		t.Errorf("vendor-only stack must yield no project frame, got %q", got)
	}
	msg := FormatExceptionForModel(ed, 1200)
	if !strings.Contains(msg, "no project-code frame in stack") {
		t.Errorf("vendor-only exception must say no project frame exists, got %q", msg)
	}
}

// TestFirstProjectFrameFindsFilesFrame pins the happy path: the first /files/
// frame is extracted with a 1-based line number and its function name, and the
// formatted message carries it.
func TestFirstProjectFrameFindsFilesFrame(t *testing.T) {
	ed := &runtime.ExceptionDetails{
		Text: "Uncaught",
		Exception: &runtime.RemoteObject{
			Description: "TypeError: Cannot read properties of null (reading 'trim')",
		},
		StackTrace: &runtime.StackTrace{CallFrames: []*runtime.CallFrame{
			frame("https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js", "onFirstUse", 20275),
			frame("http://stellar-heron.localhost:5733/files/lego.js", "boot", 152),
			frame("http://stellar-heron.localhost:5733/files/lego.js", "", 10),
		}},
	}
	want := "/files/lego.js:153 in boot"
	if got := FirstProjectFrame(ed); got != want {
		t.Errorf("FirstProjectFrame = %q, want %q", got, want)
	}
	msg := FormatExceptionForModel(ed, 1200)
	if !strings.Contains(msg, `first_project_frame: "/files/lego.js:153 in boot"`) {
		t.Errorf("formatted message must carry the project frame, got %q", msg)
	}
}

// TestFirstProjectFrameAnonymous pins the empty-function-name rendering.
func TestFirstProjectFrameAnonymous(t *testing.T) {
	ed := &runtime.ExceptionDetails{
		StackTrace: &runtime.StackTrace{CallFrames: []*runtime.CallFrame{
			frame("http://x.localhost/files/app.js", "", 4),
		}},
	}
	if got := FirstProjectFrame(ed); got != "/files/app.js:5 in (anonymous)" {
		t.Errorf("anonymous frame rendering = %q", got)
	}
}

// TestFirstProjectFrameDescriptionFallback pins the fallback when CDP delivers
// no structured stack: a /files/ line inside the rendered description (already
// 1-based) is recovered.
func TestFirstProjectFrameDescriptionFallback(t *testing.T) {
	ed := &runtime.ExceptionDetails{
		Text: "Uncaught",
		Exception: &runtime.RemoteObject{
			Description: "ReferenceError: makeBoard is not defined\n    at init (http://x.localhost:5733/files/tetris/entry.js:101:3)",
		},
	}
	if got := FirstProjectFrame(ed); got != "/files/tetris/entry.js:101" {
		t.Errorf("description fallback = %q", got)
	}
}

// TestFormatExceptionClipsRawButKeepsSuffix pins the budget behavior: a huge
// raw exception is clipped, but the project-frame suffix survives past the clip.
func TestFormatExceptionClipsRawButKeepsSuffix(t *testing.T) {
	ed := &runtime.ExceptionDetails{
		Text: "Uncaught",
		Exception: &runtime.RemoteObject{
			Description: "TypeError: boom\n" + strings.Repeat("    at deep (https://cdn.example/lib.js:1:1)\n", 100),
		},
		StackTrace: &runtime.StackTrace{CallFrames: []*runtime.CallFrame{
			frame("https://cdn.example/lib.js", "deep", 0),
			frame("http://x.localhost/files/game.js", "tick", 41),
		}},
	}
	msg := FormatExceptionForModel(ed, 1200)
	if len(msg) > 1200+220 {
		t.Errorf("formatted message blew the budget: %d bytes", len(msg))
	}
	if !strings.Contains(msg, `first_project_frame: "/files/game.js:42 in tick"`) {
		t.Errorf("clip must not eat the project-frame suffix, got tail %q", msg[len(msg)-160:])
	}
}
