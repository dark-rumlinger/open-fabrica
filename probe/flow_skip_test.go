package probe

import "testing"

// Every skip message the runner and the snapshot guard can produce maps to a tag.
func TestSkipKind(t *testing.T) {
	cases := map[string]string{
		"headless browser unavailable: no chrome":                                        SkipNoBrowser,
		`headless browser failed during flow "a": gone`:                                  SkipNoBrowser,
		"real visitors used the app in the last 10 minutes; the flows were not replayed": SkipVisitors,
		"no data snapshot is available here, so the flows were not replayed":             SkipSnapshot,
		"could not snapshot the app's data before the replay: disk full":                 SkipSnapshot,
		"flow run was cancelled before it finished (context canceled)":                   SkipCancelled,
		"no base URL configured":                                                         "",
		"":                                                                               "",
	}
	for in, want := range cases {
		if got := SkipKind(in); got != want {
			t.Errorf("SkipKind(%q) = %q, want %q", in, got, want)
		}
	}
}
