package probe

import "openfabrica/store"

// ValidationIssue is the post-build probe's finding shape, an alias of the shared
// store.Issue so the probe, the static validators, the build loop, and the write
// gates all speak one type (the loop keys on Fingerprint() to show a fixable issue at
// most once). Homed in store (a leaf) so tools/* can name it without importing engine.
type ValidationIssue = store.Issue
