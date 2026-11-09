package engine

// Tool-prose <-> registry/contract consistency guards.
//
// The doc<->registry suite scans only packs + contracts; the pack duplication
// guards scan only packs. Neither reads a tool's OWN LLM-facing prose
// (OneLiner, WhenToUse, per-action Summary, arg descriptions), so two drifts
// slip every existing check: a manage_*/find_tool name in tool prose that no
// longer names a registered tool, and a canonical contract wire-fact pasted
// into tool prose instead of transcluded (the copy-instead-of-reference case
// tools/AUTHORING.md forbids). These two tests close that half.

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
	"openfabrica/tools"
)

// toolProseSources returns every registered tool's LLM-facing prose keyed by a
// human-readable label: OneLiner + WhenToUse + each action Summary + each arg
// Description. The find_tool meta-tool is not a registered ToolDef, so its
// Description is added separately - it is prose the prompts reference by name.
func toolProseSources(t *testing.T) map[string]string {
	t.Helper()
	list := tools.DefaultRegistry().List()
	if len(list) < 20 {
		t.Fatalf("registry looks unpopulated (%d tools) - are the tool subpackages imported?", len(list))
	}
	out := map[string]string{
		"find_tool/description": tools.FindToolDef().Description,
	}
	for _, td := range list {
		var b strings.Builder
		b.WriteString(td.OneLiner)
		b.WriteString("\n")
		b.WriteString(td.WhenToUse)
		b.WriteString("\n")
		for _, meta := range td.ActionMeta {
			b.WriteString(meta.Summary)
			b.WriteString("\n")
		}
		for _, p := range td.Params {
			b.WriteString(p.Description)
			b.WriteString("\n")
		}
		out["tool/"+td.Name] = b.String()
	}
	return out
}

// TestToolProseReferencesRealTools asserts every manage_*/find_tool token in a
// tool's own prose names a registered tool. This is the surface
// TestDocsReferenceRealTools (packs + contracts only) never sees; the tokens are
// all valid today, so this passes unchanged and just ratchets against a future
// rename.
func TestToolProseReferencesRealTools(t *testing.T) {
	known, _ := registryTools(t) // adds knownExtraTools (find_tool) to the set
	for label, body := range toolProseSources(t) {
		seen := map[string]bool{}
		for _, m := range toolToken.FindAllString(body, -1) {
			if known[m] || seen[m] {
				continue
			}
			seen[m] = true
			t.Errorf("%s: references tool %q, which is not a registered tool", label, m)
		}
	}
}

// findingProseDirs are the packages whose string literals reach the model as
// BUILD ISSUES rather than prompt frames: a finding routinely names the tool
// that repairs the defect, so it is exactly as rot-prone as a guide, yet
// neither the docs scan nor the tool-prose scan reads a line of it. Walked
// rather than hand-listed so a new validator is covered the day it lands.
//
// Paths are relative to this package's directory, which is where `go test` runs.
var findingProseDirs = []string{".", "../probe", "../analysis"}

// goStringLits returns every string literal in dir's non-test Go files. Literals
// only: a comment naming a stale tool is a developer's problem, not the model's, and
// including them would price accurate history as drift.
func goStringLits(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		p := filepath.Join(dir, name)
		af, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if v, err := strconv.Unquote(bl.Value); err == nil {
					out[p] = append(out[p], v)
				}
			}
			return true
		})
	}
	return out
}

// TestFindingProseReferencesRealTools asserts every tool a validator or probe
// finding names is a registered tool. These messages are the model's instructions
// for repairing a broken build; a token that no longer resolves turns the one
// actionable sentence in a finding into a dead end. Every token is valid today, so
// this passes unchanged and ratchets against a future rename.
func TestFindingProseReferencesRealTools(t *testing.T) {
	known, _ := registryTools(t) // adds knownExtraTools (find_tool) to the set
	scanned := 0
	for _, dir := range findingProseDirs {
		for file, lits := range goStringLits(t, dir) {
			for _, lit := range lits {
				for _, m := range toolToken.FindAllString(lit, -1) {
					scanned++
					if !known[m] {
						t.Errorf("%s: a finding names tool %q, which is not a registered tool", file, m)
					}
				}
			}
		}
	}
	// A scan that silently matched nothing would pass forever while guarding
	// nothing, which is the failure mode this whole file exists to catch.
	if scanned == 0 {
		t.Fatal("no tool tokens found in any finding prose - the scan is not reading what it thinks it is")
	}
}

// minToolContractLineLen mirrors pack_duplication_test.go's minDupLen: only a
// SUBSTANTIAL contract line reproduced in tool prose is a restatement. A short
// shared phrase ("hosted checkout", "auth_token", a bare route) is normal
// vocabulary, not a pasted wire fact, so the length floor keeps false positives
// at zero.
const minToolContractLineLen = 55

// TestNoContractRestatementInToolProse asserts no tool's LLM-facing prose
// reproduces a substantial line of a canonical contract: a tool that needs a
// wire fact references its contract/guide (inlined when the tool is used); a
// pasted copy drifts. The tool-side twin of TestNoContractRestatement. It reads
// all four surfaces via toolProseSources - an arg description is the longest
// prose a tool has and the closest to the code the model is about to write,
// which is precisely where a contract gets pasted.
func TestNoContractRestatementInToolProse(t *testing.T) {
	assertNoContractRestatement(t, toolProseSources(t))
}

// TestNoContractRestatementInHints closes the last unscanned tool surface:
// Hints is a func, not a string, so toolProseSources cannot collect it, and it
// is the prose the model reads MOST (appended to the tool result as the model
// acts). renderHints already renders every body for the tool-name check; this
// feeds the same corpus through the contract check.
func TestNoContractRestatementInHints(t *testing.T) {
	assertNoContractRestatement(t, renderHints(t))
}

// assertNoContractRestatement fails for any body reproducing a substantial line of a
// canonical contract. Shared by the two callers above so both hold the same line: a
// surface that needs a wire fact references its contract or guide (which the build loop
// already inlines when the tool is used); a pasted copy drifts.
func assertNoContractRestatement(t *testing.T, sources map[string]string) {
	t.Helper()
	cs, err := contracts.List()
	if err != nil {
		t.Fatalf("contracts.List: %v", err)
	}
	type cline struct{ id, norm string }
	var lines []cline
	for _, c := range cs {
		for _, raw := range strings.Split(c.Body, "\n") {
			n := contracts.Normalize(raw)
			if len(n) >= minToolContractLineLen {
				lines = append(lines, cline{c.ID, n})
			}
		}
	}
	// Fail loudly rather than pass vacuously if either corpus looks empty (a
	// broken contract load or unpopulated registry), mirroring registryTools.
	if len(lines) < 10 {
		t.Fatalf("only %d contract lines >= %d chars - contract corpus looks empty, this guard would pass vacuously", len(lines), minToolContractLineLen)
	}
	if len(sources) == 0 {
		t.Fatal("no prose sources to scan - this guard would pass vacuously")
	}
	for label, body := range sources {
		hay := contracts.Normalize(body)
		for _, cl := range lines {
			if strings.Contains(hay, cl.norm) {
				t.Errorf("%s restates a line of contract %q in its prose instead of referencing it: %q",
					label, cl.id, clip(cl.norm, 120))
			}
		}
	}
}
