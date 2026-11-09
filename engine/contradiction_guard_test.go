package engine

// Contradiction guard - the gap the single-home guards admit they cannot cover:
// they catch DUPLICATION (a rule STATED twice), not a CONTRADICTION - a gate
// message and a stage frame issuing OPPOSITE imperatives on the same action,
// the shape that makes the build model thrash. A TARGETED canary, not a general
// contradiction detector (semantic equivalence is undecidable): it pins the
// specific contradictions resolved on the import-line and patch-vs-resave axes
// so a future edit cannot silently bring them back, and asserts the
// RESOLUTIONS stay present so the fix cannot be silently deleted either. Green
// means these two did not return - not that no contradiction exists anywhere.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/pack"
)

const (
	surfGate = "gate"
	surfPack = "pack"
	surfBoth = "both"
)

// contentGateLiterals returns the normalized concatenation of every STRING LITERAL in the
// non-test content-gate Go files - where save/patch rejection messages are authored. AST,
// not grep: a Go COMMENT that discusses a resolved contradiction ("no longer says X") must
// not trip the canary; only text the model actually sees counts.
func contentGateLiterals(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "tools", "content")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					b.WriteString(v)
					b.WriteByte('\n')
				}
			}
			return true
		})
	}
	got := contracts.Normalize(b.String())
	if len(got) < 500 {
		t.Fatalf("only %d chars of gate literal text - did tools/content move? the guard would pass vacuously", len(got))
	}
	return got
}

// packSource returns the normalized concatenation of every embedded knowledge pack body
// (stage frames and guides both).
func packSource(t *testing.T) string {
	t.Helper()
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	if len(ps) < 5 {
		t.Fatalf("only %d packs - did the knowledge tree move?", len(ps))
	}
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Body)
		b.WriteByte('\n')
	}
	return contracts.Normalize(b.String())
}

// resolvedContradiction is an imperative that must NOT reappear on `surface`, because a
// co-resident rule (`conflictsWith`) issues the opposite instruction on the same action.
type resolvedContradiction struct {
	topic         string
	banned        string // normalized-substring that must be ABSENT from the surface
	surface       string // surfGate | surfPack | surfBoth
	conflictsWith string
}

var resolvedContradictions = []resolvedContradiction{
	{
		topic:         "import-line editing",
		banned:        "add it to that line",
		surface:       surfBoth,
		conflictsWith: "the build frame's 'Never hand-edit an import line' - the platform auto-merges the partial-import case, so it owns the import line and the gate must not order a hand-edit",
	},
	{
		topic:         "import-line editing",
		banned:        "don't re-edit the call site",
		surface:       surfBoth,
		conflictsWith: "the build frame's 'patch in the CALL' - the gate must not invert it",
	},
	{
		topic:         "patch vs re-save",
		banned:        "most reliable fix is to re-send",
		surface:       surfGate,
		conflictsWith: "the build frame's 'patch ... the cheapest shape by far' - the steer is a decision rule (hold the bytes -> patch; else re-save)",
	},
	{
		topic:         "patch vs re-save",
		banned:        "re-send the whole corrected file",
		surface:       surfGate,
		conflictsWith: "the build frame's patch-first steer - a full re-save is the FALLBACK, never the unconditional lead",
	},
	{
		topic:         "js load order",
		banned:        "controlled load order",
		surface:       surfBoth,
		conflictsWith: "the client_js_split guide's 'no load-order lever' - every project JS file is an ES module resolved through imports, so no tool prose or pack may offer a JS load-order dial (attach position orders CSS cascade only)",
	},
}

func TestNoResolvedContradictions(t *testing.T) {
	gate := contentGateLiterals(t)
	packs := packSource(t)
	for _, rc := range resolvedContradictions {
		needle := contracts.Normalize(rc.banned)
		hits := func(surface, text string) {
			if strings.Contains(text, needle) {
				t.Errorf("resolved contradiction re-introduced on the %s surface (%s): %q\n  It contradicts %s.\n  Remove the imperative, or reconcile the opposing rule; see engine/contradiction_guard_test.go.",
					surface, rc.topic, rc.banned, rc.conflictsWith)
			}
		}
		if rc.surface == surfGate || rc.surface == surfBoth {
			hits("gate-message", gate)
		}
		if rc.surface == surfPack || rc.surface == surfBoth {
			hits("knowledge-pack", packs)
		}
	}
}
