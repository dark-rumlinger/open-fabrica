package all

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// notCategories are tools/ subdirectories that hold shared helpers, not tools.
var notCategories = map[string]bool{
	"all":       true,
	"internal":  true,
	"llmredact": true,
	"tooltest":  true,
}

// TestAllCategoriesImported fails when a tools/ category directory exists that
// this package does not import: its tools would silently never register.
func TestAllCategoriesImported(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "all.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	imported := map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		imported[p[strings.LastIndex(p, "/")+1:]] = true
	}

	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || notCategories[e.Name()] {
			continue
		}
		if !imported[e.Name()] {
			t.Errorf("tools/%s is not imported by tools/all: its tools never register", e.Name())
		}
		delete(imported, e.Name())
	}
	for name := range imported {
		t.Errorf("tools/all imports tools/%s, which is not a category directory", name)
	}
}
