package tools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestExtractActionFieldsRejectsDuplicateArg pins the registration-time guard:
// two fields sharing one arg name must fail extraction, because the populated
// args map keys on the arg name and would fill only one field, silently
// dropping the other (a forwarding-drift foot-gun).
func TestExtractActionFieldsRejectsDuplicateArg(t *testing.T) {
	type dupArgs struct {
		A string `arg:"path" desc:"first"`
		B string `arg:"path" desc:"second"`
	}
	if _, err := extractActionFields(reflect.TypeOf(dupArgs{})); err == nil {
		t.Fatal("expected duplicate arg name to be rejected, got nil error")
	}

	type okArgs struct {
		A string `arg:"path" desc:"first"`
		B string `arg:"name" desc:"second"`
	}
	if _, err := extractActionFields(reflect.TypeOf(okArgs{})); err != nil {
		t.Fatalf("unexpected error for distinct arg names: %v", err)
	}
}

// TestNoUnreadActionArgs is the static guard against "arg-forwarding drift": an
// action struct declares an `arg:"x"` field that its Run (or any method on the
// type) never reads, so reflection populates it and the handler silently drops it.
// Go doesn't flag an unread field and Registry.Validate only checks Examples, so
// nothing else catches it. For each Action type (a struct with a directly-declared
// arg field AND a Run method) every arg field must be referenced through the
// receiver in some method. Two conservatism valves keep false positives at zero:
//
//   - Wholesale escape: if a method uses the receiver as a value (passes `a` or
//     `&a`, ranges it, returns it), the struct may be forwarded/copied, so every
//     field is treated as consumed and the type is skipped. This is what actually
//     costs coverage: it takes out all of tools/data plus the forwarding actions in
//     api/auth/content, whose Run methods hand the receiver to a shared handler.
//   - Direct fields only: args promoted from an embedded shared arg struct are not
//     attributed to the action.
//
// The test LOGS what each valve excludes; run it with -v to see the current lists.
//
// Does NOT catch (by design): second-order drift where Run reads the field but
// forwards it under a key the downstream handler doesn't read. Guard those with a
// golden round-trip test per the tool-arg-forwarding-drift note.
func TestNoUnreadActionArgs(t *testing.T) {
	files := parseToolTree(t)
	violations, checked, seen := analyzeArgDrift(files)

	// Harness self-check: a guard that silently finds nothing to check passes
	// vacuously and protects nothing. Pin that the walk actually reached the
	// tool tree via a known sentinel and a floor on action types examined.
	if !seen["api.APICreate"] {
		t.Fatal("drift harness did not find api.APICreate with arg fields; the source walk is broken")
	}
	if checked < 20 {
		t.Fatalf("drift harness checked only %d action types with arg fields; expected many across the tool tree - the walk is likely broken", checked)
	}

	for _, v := range violations {
		t.Errorf("arg-forwarding drift: %s but Run never reads it - wire it through to the handler or drop the field", v)
	}

	// Report what the two conservatism valves cost. Both blind spots are documented
	// above, but a documented gap that nobody measures reads as a small one - and the
	// embed valve alone takes most of tools/data out of scope, which is not obvious
	// from the doc comment. Printed, never failed: closing either valve means real
	// analysis work, and blocking every unrelated change until then helps nobody.
	skippedEmbed, skippedEscape := analyzeArgDriftSkips(files)
	t.Logf("checked %d action types; NOT checked: %d with only embedded/promoted args, "+
		"%d whose receiver escapes as a value", checked, len(skippedEmbed), len(skippedEscape))
	if len(skippedEmbed) > 0 {
		sort.Strings(skippedEmbed)
		t.Logf("  embedded-args (no direct arg field, so nothing is attributed): %s", strings.Join(skippedEmbed, ", "))
	}
	if len(skippedEscape) > 0 {
		sort.Strings(skippedEscape)
		t.Logf("  receiver-escapes (fields treated as consumed wholesale): %s", strings.Join(skippedEscape, ", "))
	}
}

// TestArgDriftDetectsUnreadField is the negative control proving the guard
// actually fires: a synthetic action whose Run reads one arg and ignores
// another must flag exactly the ignored one.
func TestArgDriftDetectsUnreadField(t *testing.T) {
	src := "package fake\n" +
		"type FakeAct struct {\n" +
		"\tUsed   string `arg:\"used\" desc:\"d\"`\n" +
		"\tUnused string `arg:\"unused\" desc:\"d\"`\n" +
		"}\n" +
		"func (a *FakeAct) Run(ctx interface{}) error { _ = a.Used; return nil }\n"
	f, err := parser.ParseFile(token.NewFileSet(), "fake.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	violations, _, _ := analyzeArgDrift([]*ast.File{f})
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "fake.FakeAct") || !strings.Contains(joined, "(field Unused)") {
		t.Errorf("expected drift flagged on fake.FakeAct unused arg, got: %v", violations)
	}
	if strings.Contains(joined, "(field Used)") {
		t.Errorf("read field Used was wrongly flagged: %v", violations)
	}
}

// TestArgDriftEscapeValve proves the wholesale-escape valve: when Run forwards
// the receiver as a value (the struct may be reflected/copied), no field is
// flagged even if it is not read field-by-field.
func TestArgDriftEscapeValve(t *testing.T) {
	src := "package fake\n" +
		"type FakeEsc struct {\n" +
		"\tX string `arg:\"x\" desc:\"d\"`\n" +
		"}\n" +
		"func sink(v interface{}) {}\n" +
		"func (a *FakeEsc) Run(ctx interface{}) error { sink(a); return nil }\n"
	f, err := parser.ParseFile(token.NewFileSet(), "fake.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	if v, _, _ := analyzeArgDrift([]*ast.File{f}); len(v) != 0 {
		t.Errorf("escape valve failed: receiver forwarded wholesale should suppress flags, got: %v", v)
	}
}

// analyzeArgDriftSkips reports which Action types the drift scan does NOT examine, split
// by which valve excluded them: types whose args are all promoted from an embedded struct
// (never attributed to the action), and types whose Run forwards the receiver as a value
// (fields untrackable, so all are treated as read).
//
// It re-walks rather than threading the two lists out of analyzeArgDrift, which is kept
// free of *testing.T and reporting concerns so it can run on synthetic ASTs. The
// duplication is a few lines and buys the detection core staying a pure function.
func analyzeArgDriftSkips(files []*ast.File) (embedded, escaped []string) {
	hasDirectArg := map[string]bool{}
	hasEmbeddedStruct := map[string]bool{}
	hasRun := map[string]bool{}
	type methodInfo struct {
		recvName string
		body     *ast.BlockStmt
	}
	methods := map[string][]methodInfo{}

	recvTypeName := func(e ast.Expr) string {
		if s, ok := e.(*ast.StarExpr); ok {
			e = s.X
		}
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}

	for _, f := range files {
		pkg := f.Name.Name
		for _, decl := range f.Decls {
			switch dcl := decl.(type) {
			case *ast.GenDecl:
				if dcl.Tok != token.TYPE {
					continue
				}
				for _, spec := range dcl.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						continue
					}
					key := pkg + "." + ts.Name.Name
					for _, fld := range st.Fields.List {
						if len(fld.Names) == 0 {
							hasEmbeddedStruct[key] = true
							continue
						}
						if fld.Tag == nil {
							continue
						}
						if _, ok := reflect.StructTag(strings.Trim(fld.Tag.Value, "`")).Lookup("arg"); ok {
							hasDirectArg[key] = true
						}
					}
				}
			case *ast.FuncDecl:
				if dcl.Recv == nil || len(dcl.Recv.List) == 0 {
					continue
				}
				rt := recvTypeName(dcl.Recv.List[0].Type)
				if rt == "" {
					continue
				}
				key := pkg + "." + rt
				recvName := ""
				if names := dcl.Recv.List[0].Names; len(names) > 0 {
					recvName = names[0].Name
				}
				methods[key] = append(methods[key], methodInfo{recvName: recvName, body: dcl.Body})
				if dcl.Name.Name == "Run" {
					hasRun[key] = true
				}
			}
		}
	}

	for key := range hasRun {
		if !hasDirectArg[key] {
			if hasEmbeddedStruct[key] {
				embedded = append(embedded, key)
			}
			continue
		}
		for _, m := range methods[key] {
			if m.recvName == "" || m.recvName == "_" || m.body == nil {
				continue
			}
			accounted := map[*ast.Ident]bool{}
			esc := false
			ast.Inspect(m.body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == m.recvName {
						accounted[id] = true
					}
				case *ast.Ident:
					if x.Name == m.recvName && !accounted[x] {
						esc = true
					}
				}
				return true
			})
			if esc {
				escaped = append(escaped, key)
				break
			}
		}
	}
	return embedded, escaped
}

// parseToolTree parses every non-test .go file under the tools/ tree into a
// flat slice of ASTs (used by the repo-wide drift scan).
func parseToolTree(t *testing.T) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return files
}

// analyzeArgDrift is the pure detection core: given parsed files, it returns one
// violation string per directly-declared arg field on an Action struct that the
// type's methods never read through the receiver, plus the count of action
// types examined and the set of examined type keys (pkg.Type). Kept free of
// *testing.T so it can be exercised on synthetic ASTs.
func analyzeArgDrift(files []*ast.File) (violations []string, checkedTypes int, seen map[string]bool) {
	type argField struct{ goName, argName string }
	type methodInfo struct {
		recvName string
		body     *ast.BlockStmt
	}
	structArgs := map[string][]argField{}
	hasRun := map[string]bool{}
	methods := map[string][]methodInfo{}
	seen = map[string]bool{}

	recvTypeName := func(e ast.Expr) string {
		if s, ok := e.(*ast.StarExpr); ok {
			e = s.X
		}
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}

	for _, f := range files {
		pkg := f.Name.Name
		for _, decl := range f.Decls {
			switch dcl := decl.(type) {
			case *ast.GenDecl:
				if dcl.Tok != token.TYPE {
					continue
				}
				for _, spec := range dcl.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						continue
					}
					key := pkg + "." + ts.Name.Name
					for _, fld := range st.Fields.List {
						if len(fld.Names) == 0 || fld.Tag == nil {
							continue // embedded field or untagged: not a direct arg
						}
						tag := reflect.StructTag(strings.Trim(fld.Tag.Value, "`"))
						argTag, ok := tag.Lookup("arg")
						if !ok {
							continue
						}
						argName := strings.TrimSpace(strings.Split(argTag, ",")[0])
						if argName == "" {
							continue
						}
						for _, nm := range fld.Names {
							if !ast.IsExported(nm.Name) {
								continue
							}
							structArgs[key] = append(structArgs[key], argField{goName: nm.Name, argName: argName})
						}
					}
				}
			case *ast.FuncDecl:
				if dcl.Recv == nil || len(dcl.Recv.List) == 0 {
					continue
				}
				rt := recvTypeName(dcl.Recv.List[0].Type)
				if rt == "" {
					continue
				}
				key := pkg + "." + rt
				recvName := ""
				if names := dcl.Recv.List[0].Names; len(names) > 0 {
					recvName = names[0].Name
				}
				methods[key] = append(methods[key], methodInfo{recvName: recvName, body: dcl.Body})
				if dcl.Name.Name == "Run" {
					hasRun[key] = true
				}
			}
		}
	}

	for key, fields := range structArgs {
		if !hasRun[key] || len(fields) == 0 {
			continue // not an Action, or an Action with only embedded/promoted args
		}
		checkedTypes++
		seen[key] = true

		read := map[string]bool{}
		escaped := false
		for _, m := range methods[key] {
			if m.recvName == "" || m.recvName == "_" || m.body == nil {
				continue
			}
			// Pre-order traversal visits a SelectorExpr before its X child, so a
			// receiver Ident used as `recv.Field` is recorded as accounted before
			// we reach the Ident itself. Any receiver Ident that is NOT a selector
			// base is the struct escaping as a value (forwarded, copied, addressed)
			// - then field use is untrackable and we treat all fields as read.
			accounted := map[*ast.Ident]bool{}
			ast.Inspect(m.body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == m.recvName {
						read[x.Sel.Name] = true
						accounted[id] = true
					}
				case *ast.Ident:
					if x.Name == m.recvName && !accounted[x] {
						escaped = true
					}
				}
				return true
			})
		}
		if escaped {
			continue
		}
		for _, fld := range fields {
			if !read[fld.goName] {
				violations = append(violations, fmt.Sprintf("%s arg %q (field %s)", key, fld.argName, fld.goName))
			}
		}
	}
	return violations, checkedTypes, seen
}
