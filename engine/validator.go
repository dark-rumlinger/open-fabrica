package engine

// The pluggable validator layer. A Validator inspects the built project and
// returns issues; the build stage runs them once after the build and ships
// what they find as known issues. Adding one =
// RegisterValidator, a self-registering file under engine/validators. Two tiers:
// STATIC needs no browser and reuses the save-time write gates' primitives so a
// finding means the same on save and build; RUNTIME loads each page in headless
// Chrome for what only errors at load time. Every issue carries Fingerprint() so
// the loop presents it at most once.

import (
	"context"
	"database/sql"
	"sync"

	"openfabrica/probe"
)

// Issue is one validator finding. Alias of the probe package's shape so the
// existing issue plumbing (category/page/file/severity) is the contract.
type Issue = probe.ValidationIssue

// Validator inspects the running project and reports issues.
type Validator interface {
	Name() string
	// Validate probes the project served at baseURL (hostHeader carries the
	// project's vhost when set). Returns error-severity issues only.
	Validate(ctx context.Context, pdb *sql.DB, baseURL, hostHeader string) []Issue
}

// SequentialValidator is a validator that must not overlap another one: it writes
// through the app under a data snapshot, and two snapshot-and-restore cycles at once
// would undo each other's restore. These run one at a time after the parallel batch.
type SequentialValidator interface {
	Validator
	Sequential()
}

var (
	validatorMu  sync.RWMutex
	validatorSet = []Validator{}
)

// RegisterValidator appends a validator to the post-build set.
func RegisterValidator(v Validator) {
	validatorMu.Lock()
	defer validatorMu.Unlock()
	validatorSet = append(validatorSet, v)
}

// validators returns the registered set (copy).
func validators() []Validator {
	validatorMu.RLock()
	defer validatorMu.RUnlock()
	out := make([]Validator, len(validatorSet))
	copy(out, validatorSet)
	return out
}
