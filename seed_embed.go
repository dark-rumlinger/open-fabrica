// Package openfabrica carries files that ship inside the binary.
package openfabrica

import _ "embed"

// SeedJSON is the curated provider seed, used when no seed.json sits on disk so
// a bare binary or container still starts with its providers.
//
//go:embed seed.json
var SeedJSON []byte
