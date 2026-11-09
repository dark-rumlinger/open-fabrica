package engine

// Guard: every [mode: X] tag the engine emits into a stage prompt must be
// documented as a `## `[mode: X]`` section in some stage frame, so a mode added
// in code but not in a frame fails the build (the model must not interpret an
// undocumented mode against rules that do not fit it). Presence, not which
// frame, on purpose: "emitted but documented nowhere" is the real failure.
// AST-scanning string literals (not grep) so a mode named only in a Go comment
// does not count as emitted.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"openfabrica/knowledge"
)

var (
	modeTagRe     = regexp.MustCompile(`\[mode:\s*([a-z][a-z-]*)\]`)
	modeHeadingRe = regexp.MustCompile("(?m)^##\\s+`\\[mode:\\s*([a-z][a-z-]*)\\]`")
)

// emittedModeTags AST-walks the engine package's own non-test Go sources and
// returns the mode tokens that appear inside STRING LITERALS (the ones that reach a
// prompt), mapped to the file:line where each was first seen.
func emittedModeTags(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range modeTagRe.FindAllStringSubmatch(v, -1) {
				if _, seen := out[m[1]]; !seen {
					out[m[1]] = name + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line)
				}
			}
			return true
		})
	}
	return out
}

// documentedModeTags returns the mode tokens that carry a `## `[mode: X]“ heading
// in some stage frame under knowledge/stages.
func documentedModeTags(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := fs.WalkDir(knowledge.FS, "stages", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(p) != "STAGE.md" {
			return nil
		}
		b, err := fs.ReadFile(knowledge.FS, p)
		if err != nil {
			return err
		}
		for _, m := range modeHeadingRe.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk stages: %v", err)
	}
	return out
}

func TestEmittedModesAreDocumented(t *testing.T) {
	emitted := emittedModeTags(t)
	documented := documentedModeTags(t)
	if len(emitted) == 0 {
		t.Fatal("found no [mode: X] tags in engine string literals; the scan is broken")
	}
	var missing []string
	for mode, where := range emitted {
		if !documented[mode] {
			missing = append(missing, mode+" (emitted at "+where+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("engine emits [mode: X] tags documented in no stage frame; add a '## `[mode: X]`' section to the matching knowledge/stages/*/STAGE.md:\n  %s",
			strings.Join(missing, "\n  "))
	}
}
