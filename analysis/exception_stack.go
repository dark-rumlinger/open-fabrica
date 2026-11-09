package analysis

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/chromedp/cdproto/runtime"

	"openfabrica/internal/stringutil"
)

// Exception formatting for the browser probes. A raw CDP exception clipped too
// hard can leave only vendor-internal frames, pointing the model at the wrong
// file; these helpers keep a wider budget and pull out the first /files/ frame.

// descFilesFrame matches a "/files/<path>:<line>" occurrence inside an already
// 1-based rendered stack line (the fallback when no structured trace exists).
var descFilesFrame = regexp.MustCompile(`(/files/[^\s):]+):(\d+)`)

// FirstProjectFrame returns the first stack frame of ed that points into the
// project's own /files/ modules, formatted "/files/lego.js:153 in boot", or ""
// when the whole stack is library/vendor-internal. Structured CDP frames carry
// 0-based line numbers; the rendered-description fallback is already 1-based.
func FirstProjectFrame(ed *runtime.ExceptionDetails) string {
	if ed == nil {
		return ""
	}
	if ed.StackTrace != nil {
		for _, f := range ed.StackTrace.CallFrames {
			if f == nil {
				continue
			}
			idx := strings.Index(f.URL, "/files/")
			if idx < 0 {
				continue
			}
			fn := f.FunctionName
			if fn == "" {
				fn = "(anonymous)"
			}
			return fmt.Sprintf("%s:%d in %s", f.URL[idx:], f.LineNumber+1, fn)
		}
	}
	if ed.Exception != nil {
		if m := descFilesFrame.FindStringSubmatch(ed.Exception.Description); m != nil {
			return m[1] + ":" + m[2]
		}
	}
	return ""
}

// FormatExceptionForModel renders ed for a build-model-facing probe result: the
// raw exception within rawBudget bytes, then the first project-code frame, or an
// explicit no-project-frame note that steers the model away from patching the library.
func FormatExceptionForModel(ed *runtime.ExceptionDetails, rawBudget int) string {
	if ed == nil {
		return ""
	}
	raw := strings.TrimSpace(ed.Error())
	if rawBudget > 0 {
		raw = stringutil.Truncate(raw, rawBudget)
	}
	if frame := FirstProjectFrame(ed); frame != "" {
		return raw + fmt.Sprintf("\nfirst_project_frame: %q - start from this file/line; frames above it are library-internal.", frame)
	}
	return raw + "\nno project-code frame in stack - the throw happens inside a library call; look at what your code passes INTO the library (a null/missing element, wrong argument types, too many renderer/context creations) and at what runs during boot, not at the library file itself."
}
