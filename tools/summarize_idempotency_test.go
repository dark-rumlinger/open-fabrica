package tools_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"openfabrica/tools"

	// Populate DefaultRegistry: tools register from each package's init.
	_ "openfabrica/tools/all" // register every tool category via init()
)

// History compaction re-applies a tool's Summarize hook to its own output
// (documented on summarizeClip in summarize.go): a hook that returns different
// bytes on re-application rewrites a cached mid-history message every pass and
// re-bills the whole suffix. A hook that clips fall-through and unparseable output
// at different lengths shrinks a clipped summary again; this pins the contract for
// every registered hook at once.
func TestEverySummarizeHookIsIdempotent(t *testing.T) {
	long := strings.Repeat("x", 400)
	multibyte := strings.Repeat("héllo wörld ", 40)
	fixtures := map[string]string{
		"unparseable long":      "raw non-json output " + long,
		"unparseable multibyte": multibyte,
		"failure with code":     `{"success":false,"error":"` + long[:200] + `","code":"some_reject"}`,
		"list result":           `{"success":true,"data":[1,2,3,4,5]}`,
		"map with common keys":  `{"success":true,"data":{"name":"a","table":"t","endpoint_path":"p","columns":[1,2]}}`,
		"map fall-through long": `{"success":true,"data":{"blob":"` + long + `"}}`,
		"scalar data long":      `{"success":true,"data":"` + long + `"}`,
		"mutation inserted":     `{"success":true,"data":{"inserted":3,"table":"t"}}`,
	}
	seen := 0
	for _, td := range tools.DefaultRegistry().List() {
		if td.Summarize == nil {
			continue
		}
		seen++
		for name, x := range fixtures {
			once := td.Summarize(x)
			twice := td.Summarize(once)
			if once != twice {
				t.Errorf("%s: Summarize is not idempotent on %q:\n  once:  %q\n  twice: %q\n"+
					"  compaction re-applies hooks to their own output; use one clip length in every branch (tools.SummarizeFrame)",
					td.Name, name, once, twice)
			}
			if !utf8.ValidString(once) {
				t.Errorf("%s: Summarize emitted invalid UTF-8 on %q - clip with stringutil.Truncate, not a byte slice", td.Name, name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no registered tool carries a Summarize hook; the guard is vacuous")
	}
}
