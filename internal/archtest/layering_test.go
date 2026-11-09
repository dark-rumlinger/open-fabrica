package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "openfabrica/"

// moduleImports maps every non-test package directory (slash path relative to
// the module root) to the module-internal packages its production files import.
// Test files are left out on purpose: a test may reach anywhere it needs to.
func moduleImports(t *testing.T) map[string][]string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "data", "web", "node_modules", "testdata", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pkg := filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, module) {
				out[pkg] = append(out[pkg], strings.TrimPrefix(p, module))
			}
		}
		if _, ok := out[pkg]; !ok {
			out[pkg] = nil
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// toolCategories are the tool packages tools/all registers: the LLM-facing
// surface, which nothing outside tools/ may depend on.
func toolCategories(t *testing.T, imports map[string][]string) map[string]bool {
	cats := map[string]bool{}
	for _, p := range imports["tools/all"] {
		cats[p] = true
	}
	if len(cats) < 5 {
		t.Fatalf("tools/all registers %d categories; the scan is broken", len(cats))
	}
	return cats
}

func under(pkg, prefix string) bool { return pkg == prefix || strings.HasPrefix(pkg, prefix+"/") }

// TestLayering holds the dependency direction the architecture relies on:
//
//   - Tool categories are the model-facing surface. Runtime code (the public
//     server, jobs, triggers, the engine) shares domain logic through packages
//     like billing, security, internal/netguard, and internal/endpoints, never by
//     importing a tool. Only cmd (via tools/all) loads them.
//   - Tools never import the engine, the servers, or the runtime subsystems that
//     call them; they reach shared behaviour through leaf packages (probe, store).
//   - Leaf packages that name shared stored values import nothing from the module,
//     so anyone may depend on them.
//   - billing is a domain package: it may not depend on tools or on its callers.
func TestLayering(t *testing.T) {
	imports := moduleImports(t)
	cats := toolCategories(t, imports)

	callerSide := []string{"engine", "server", "chat", "jobs", "triggers", "bot", "webhooks", "cmd"}
	leaves := []string{"internal/buildstate", "internal/endpoints", "internal/dbh"}

	var errs []string
	pkgs := make([]string, 0, len(imports))
	for p := range imports {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		for _, dep := range imports[pkg] {
			switch {
			case cats[dep] && !under(pkg, "tools"):
				errs = append(errs, pkg+" imports tool package "+dep+": move the shared logic into a domain or internal package")
			case under(pkg, "tools"):
				for _, c := range callerSide {
					if under(dep, c) {
						errs = append(errs, pkg+" imports "+dep+": tools must not depend on their callers")
					}
				}
			}
			if under(pkg, "billing") && (under(dep, "tools") || under(dep, "server") || under(dep, "engine")) {
				errs = append(errs, "billing imports "+dep+": the payments domain may not depend on its callers")
			}
		}
		for _, leaf := range leaves {
			if pkg == leaf && len(imports[pkg]) > 0 {
				errs = append(errs, leaf+" must stay a leaf but imports "+strings.Join(imports[pkg], ", "))
			}
		}
	}
	for _, e := range errs {
		t.Error(e)
	}
}

// The scan must actually see the tree it guards.
func TestLayeringScanSeesTheModule(t *testing.T) {
	imports := moduleImports(t)
	for _, want := range []string{"engine", "server/public", "tools/api", "billing", "probe"} {
		if _, ok := imports[want]; !ok {
			t.Errorf("scan did not find package %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", "go.mod")); err != nil {
		t.Fatalf("archtest must run two levels below the module root: %v", err)
	}
}
