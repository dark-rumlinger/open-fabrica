package api

import (
	"strings"
	"testing"

	"openfabrica/analysis"
)

// The platform helper bodies must parse as valid ES modules: they are emitted with
// is_module=true and imported by specifier ('ofnet'/'e2ee'), so a syntax slip here
// ships a helper that throws on load with no other gate to catch it.
func TestPlatformHelpers_ParseAsESModules(t *testing.T) {
	cases := []struct{ path, body string }{
		{"js/ofnet.js", ofnetHelperJS},
		{"js/e2ee.js", e2eeHelperJS},
	}
	for _, c := range cases {
		if rep := analysis.JSSyntax(c.path, c.body); !rep.Clean {
			t.Errorf("%s does not parse as an ES module: %s", c.path, rep.Detail)
		}
		// The embedded body is minified, so the export may be serialized as
		// `export{...}` (no space) or `export ...`; accept either form.
		if !strings.Contains(c.body, "export{") && !strings.Contains(c.body, "export ") {
			t.Errorf("%s must carry a top-level export to be treated as an ES module", c.path)
		}
	}
}
