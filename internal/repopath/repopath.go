// Package repopath locates the repository root so build-time generators resolve
// the same paths no matter where they are invoked from: `go generate ./knowledge`
// (cwd = the package dir), `go run openfabrica/internal/assetgen` from release.sh
// (cwd = repo root), or anything else. Without this the generators would depend on
// the caller's working directory and quietly produce nothing or the wrong tree.
package repopath

import (
	"errors"
	"os"
	"path/filepath"
)

// Root walks up from the current working directory until it finds the directory
// holding go.mod, and returns it. It errors if none is found.
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repopath: go.mod not found in any parent of the working directory")
		}
		dir = parent
	}
}
