package auth

import (
	"strings"
	"testing"

	"openfabrica/analysis"
)

// The auth bootstrap must parse as a valid ES module: it is emitted with
// is_module=true and imported by specifier ('auth'). The __AUTH_PATH__ placeholder
// sits inside a string literal, so the raw constant parses as-is.
func TestAuthBootstrap_ParsesAsESModule(t *testing.T) {
	if rep := analysis.JSSyntax("js/auth.js", authBootstrapJS); !rep.Clean {
		t.Errorf("js/auth.js does not parse as an ES module: %s", rep.Detail)
	}
	// The embedded body is minified, so the export may be serialized as
	// `export{...}` (no space) or `export ...`; accept either form.
	if !strings.Contains(authBootstrapJS, "export{") && !strings.Contains(authBootstrapJS, "export ") {
		t.Error("js/auth.js must carry a top-level export to be treated as an ES module")
	}
}
