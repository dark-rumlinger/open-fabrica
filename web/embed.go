// Package web embeds the admin UI assets so the binary serves the control panel
// with no files on disk. Keeping the embed here (rather than in the command
// package) makes it independent of the binary's location in the module.
package web

import "embed"

//go:generate go run openfabrica/internal/assetgen

// AdminFS is the embedded admin single-page app. It serves the minified tree
// (admin-min/), generated from the plaintext admin/ sources by internal/assetgen
// (run via `go generate ./web`). The plaintext admin/ tree stays on disk as what
// developers edit and what the dev build serves from disk; only the minified copy
// ships in the binary. adminAssets in the command package roots callers at
// admin-min/, so paths are unchanged.
//
//go:embed all:admin-min
var AdminFS embed.FS
