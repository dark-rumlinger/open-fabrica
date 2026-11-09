package content

import "strings"

// contains is the shared substring assertion helper for this package's tests.
func contains(s, sub string) bool { return strings.Contains(s, sub) }
