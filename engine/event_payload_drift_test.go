package engine

// Drift guard: a project-facing event's payload keys must be the ones the
// trigger-events contract documents. A trigger template reads `{{key}}` from the
// payload; a key the code emits under another name expands to nothing, silently.
//
// The check reads every non-test `events.NewEvent(events.EventX, _, map[string]any{...})`
// call with a literal payload and asserts each string-literal key appears in that
// event's contract row. Payloads built elsewhere (a helper's map, a computed key) are
// out of reach of a source scan and are skipped.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"openfabrica/contracts"
)

func TestProjectFacingEventPayloadKeysAreDocumented(t *testing.T) {
	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	c, ok := all["trigger-events"]
	if !ok {
		t.Fatal("trigger-events contract not found")
	}
	rowFor := func(name string) string {
		for _, line := range strings.Split(c.Body, "\n") {
			if strings.HasPrefix(line, "|") && strings.Contains(strings.SplitN(line[1:], "|", 2)[0], "`"+name+"`") {
				return line
			}
		}
		return ""
	}

	root := moduleRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "events", "types.go"))
	if err != nil {
		t.Fatalf("reading events/types.go: %v", err)
	}
	constNames := map[string]string{}
	for _, m := range regexp.MustCompile(`(Event\w+)\s+EventType\s*=\s*"([a-z][a-z._]*)"`).FindAllStringSubmatch(string(src), -1) {
		constNames[m[1]] = m[2]
	}
	fset := token.NewFileSet()
	checked := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "data":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		af, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fn.Sel.Name != "NewEvent" {
				return true
			}
			typ, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name, ok := constNames[typ.Sel.Name]
			if !ok || !isProjectFacingEvent(name) {
				return true
			}
			lit, ok := call.Args[2].(*ast.CompositeLit)
			if !ok {
				return true
			}
			row := rowFor(name)
			if row == "" {
				t.Errorf("%s publishes %q, which has no row in the trigger-events contract", fset.Position(call.Pos()), name)
				return true
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				bl, ok := kv.Key.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				key, _ := strconv.Unquote(bl.Value)
				checked++
				if !strings.Contains(row, "`"+key+"`") {
					t.Errorf("%s: %q payload key %q is not in its trigger-events contract row", fset.Position(kv.Pos()), name, key)
				}
			}
			return true
		})
		return nil
	})
	if checked < 10 {
		t.Fatalf("checked only %d payload keys - did the publish call shape change?", checked)
	}
}

func isProjectFacingEvent(name string) bool {
	for _, p := range projectFacingPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
