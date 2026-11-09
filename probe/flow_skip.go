package probe

import "strings"

// Why a flow replay did not run, as a short stable tag for the admin UI to word.
const (
	SkipNoBrowser = "no_browser"
	SkipVisitors  = "visitors"
	SkipSnapshot  = "snapshot"
	SkipCancelled = "cancelled"
)

// FlowSkippedSignaturePrefix starts the signature of the validator finding raised
// when the replay did not run; the skip kind (possibly empty) follows it.
const FlowSkippedSignaturePrefix = "flow|skipped|"

// SkipKind classifies a FlowResult.Skipped message. The messages come from the
// runner and from the data snapshot guard, which word them for the model, so the
// UI reads this tag instead of the prose. An unrecognised message gives "".
func SkipKind(skipped string) string {
	s := strings.ToLower(skipped)
	switch {
	case strings.Contains(s, "headless browser"):
		return SkipNoBrowser
	case strings.Contains(s, "real visitors"):
		return SkipVisitors
	case strings.Contains(s, "snapshot"):
		return SkipSnapshot
	case strings.Contains(s, "cancelled"):
		return SkipCancelled
	}
	return ""
}
