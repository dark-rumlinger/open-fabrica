package stringutil

// Go-helper single-home guard. Every duplication guard in the repo scans
// prose (packs, contracts, tool prose); none scanned Go, which is how NINE
// ad-hoc clip helpers accumulated - eight of them byte-slicing (the invalid
// UTF-8 bug Truncate's doc warns about) and two disagreeing on whether the cap
// includes the ellipsis. A narrow AST detector keeps the class dead: a
// clip-shaped return (string slice + ellipsis literal) outside this package.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func repoGoFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// Tool/worktree directories (e.g. .claude) can hold full checkouts of other
			// revisions whose files are not this tree's to police.
			case ".git", ".claude", "node_modules", "web", "testdata", "data", "bin", "release":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(files) < 50 {
		t.Fatalf("found only %d Go files; the walk root is wrong and the guard is vacuous", len(files))
	}
	return files
}

// ellipsisLit reports whether a string literal is a truncation marker.
func ellipsisLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	return strings.Contains(lit.Value, "...") || strings.Contains(lit.Value, "…")
}

// TestNoClipShapedHelpersOutsideStringutil: `return s[:n] + "..."` only
// type-checks on strings, so the AST shape (slice + ellipsis literal in one
// concatenation) IS a re-implemented clip. Use Truncate instead.
func TestNoClipShapedHelpersOutsideStringutil(t *testing.T) {
	fset := token.NewFileSet()
	for _, path := range repoGoFiles(t) {
		if strings.Contains(filepath.ToSlash(path), "internal/stringutil/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue // generated or exotic file; not this guard's business
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || bin.Op != token.ADD {
				return true
			}
			if _, isSlice := bin.X.(*ast.SliceExpr); isSlice && ellipsisLit(bin.Y) {
				pos := fset.Position(bin.Pos())
				t.Errorf("%s:%d re-implements string clipping (slice + ellipsis literal) - use "+
					"stringutil.Truncate, which is rune-safe and idempotent", pos.Filename, pos.Line)
			}
			return true
		})
	}
}
