//go:build !dev

package main

import (
	"io/fs"

	"openfabrica/web"
)

// adminAssets returns the embedded admin UI. Production builds serve it from the
// binary via the web package's embed. The dev build tag swaps in a disk-backed
// variant (see embed_dev.go).
func adminAssets() (fs.FS, error) {
	return fs.Sub(web.AdminFS, "admin-min")
}
