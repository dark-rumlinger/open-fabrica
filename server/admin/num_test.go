package admin

// itoa renders a small non-negative int for test URLs. Single digit only, which
// is all the admin handler tests need (relocated here when triage moved out).
func itoa(n int) string { return string(rune('0' + n)) }
