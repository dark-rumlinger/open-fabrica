//go:build dev

package main

import (
	"io/fs"
	"os"
)

// adminAssets serves the admin UI from disk (web/admin) so edits appear on a
// reload without rebuilding the binary. Enabled by the dev build tag, which
// `make dev` (go run -tags dev .) passes; the path is relative to the working
// directory, so run from the repository root.
func adminAssets() (fs.FS, error) {
	return os.DirFS("web/admin"), nil
}
