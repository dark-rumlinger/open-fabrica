package all

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"openfabrica/tools"
)

// TestToolFileNameCategoryAgree holds the docs/CONVENTIONS.md rule that a tool
// lives in tools/{Category}/{Name}.go: the file name, the registered Name, and
// the Category (its directory) all agree, so a tool is found where its name says.
func TestToolFileNameCategoryAgree(t *testing.T) {
	defs := tools.DefaultRegistry().List()
	if len(defs) < 40 {
		t.Fatalf("registry holds %d tools; tools/all should load the full catalogue", len(defs))
	}
	for _, d := range defs {
		if d.SourceFile == "" {
			t.Errorf("tool %s has no SourceFile: register it through tools.Define", d.Name)
			continue
		}
		file := filepath.Base(d.SourceFile)
		dir := filepath.Base(filepath.Dir(d.SourceFile))
		if file != d.Name+".go" {
			t.Errorf("tool %s is declared in %s, want %s.go", d.Name, file, d.Name)
		}
		if dir != d.Category {
			t.Errorf("tool %s has Category %q but lives in tools/%s", d.Name, d.Category, dir)
		}
	}
}

// handDDL matches a test creating a platform table itself.
var handDDL = regexp.MustCompile(`CREATE TABLE (IF NOT EXISTS )?of_`)

// TestNoHandWrittenPlatformDDLInToolTests keeps tool tests on the real schema:
// a hand-copied of_* table drifts from db/project_migrations while the test
// stays green. Use tools/tooltest (ProjectDB, or New for a full harness).
func TestNoHandWrittenPlatformDDLInToolTests(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if handDDL.Match(body) {
			t.Errorf("%s creates an of_* table by hand; use tooltest.ProjectDB for the migrated schema", filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDefaultRegistryValidates runs the boot-time structural check under go
// test, over exactly the catalogue the binary registers.
func TestDefaultRegistryValidates(t *testing.T) {
	for _, e := range tools.RegistrationErrors() {
		t.Errorf("tool registration error: %v", e)
	}
	if err := tools.DefaultRegistry().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestWireSchemasAreProviderSafe lints every registered tool's parameter schema
// for shapes some providers reject outright (and a rejection fails the whole
// tools array, not one call): an array with no items, and a tool name outside the
// portable pattern or length.
func TestWireSchemasAreProviderSafe(t *testing.T) {
	namePattern := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for _, d := range tools.DefaultRegistry().List() {
		if !namePattern.MatchString(d.Name) {
			t.Errorf("tool name %q is outside [a-zA-Z0-9_-]{1,64}", d.Name)
		}
		var walk func(path string, v any)
		walk = func(path string, v any) {
			m, ok := v.(map[string]any)
			if !ok {
				return
			}
			if m["type"] == "array" {
				if _, has := m["items"]; !has {
					t.Errorf("%s: array at %s has no items", d.Name, path)
				}
			}
			if props, ok := m["properties"].(map[string]any); ok {
				for k, p := range props {
					walk(path+"."+k, p)
				}
			}
			if it, ok := m["items"]; ok {
				walk(path+"[]", it)
			}
		}
		walk("params", d.Parameters())
	}
}
