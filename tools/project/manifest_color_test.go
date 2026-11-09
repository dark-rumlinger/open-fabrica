package project

import "testing"

// The manifest writes these colours through verbatim, so only a literal a browser
// accepts may pass. A planner annotation must fall back to the caller's default
// rather than ship a manifest colour nothing can parse.
func TestManifestColor(t *testing.T) {
	for _, tc := range []struct {
		in, want, why string
	}{
		{"#2563eb", "#2563eb", "six-digit hex"},
		{"#abc", "#abc", "short hex"},
		{"#aabbccdd", "#aabbccdd", "hex with alpha"},
		{"  #050208  ", "#050208", "surrounding space"},
		{"#d8b4b8 (rose quartz - hover)", "", "annotated hex is rejected"},
		{"rebeccapurple", "", "named colour is rejected"},
		{"rgb(20,20,20)", "", "function value is rejected"},
		{"#12345", "", "wrong digit count"},
		{"#", "", "bare hash"},
		{"", "", "empty"},
	} {
		if got := manifestColor(tc.in); got != tc.want {
			t.Errorf("%s: manifestColor(%q) = %q, want %q", tc.why, tc.in, got, tc.want)
		}
	}
}
