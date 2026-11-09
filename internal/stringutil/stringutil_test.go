package stringutil

import (
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		// Multibyte: fewer runes than bytes, so no clipping at rune count.
		{"héllo", 5, "héllo"},
		{"héllo wörld", 7, "héllo w..."},
		{"日本語のテキスト", 3, "日本語..."},
	}
	for _, c := range cases {
		got := Truncate(c.in, c.max)
		if got != c.want {
			t.Errorf("Truncate(%q, %d) = %q; want %q", c.in, c.max, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d) produced invalid UTF-8: %q", c.in, c.max, got)
		}
	}
}

// Truncate is the single home for clipping (nine ad-hoc byte-slice copies were
// consolidated onto it); these are the properties every caller now inherits.
func TestTruncateProperties(t *testing.T) {
	inputs := []string{
		"plain ascii text that goes on for a while and then some more",
		"héllo wörld héllo wörld héllo wörld héllo wörld",
		"日本語のテキストが続く日本語のテキストが続く",
		"mixed ascii と 日本語 and émoji 🎉🎉🎉 tail",
	}
	for _, in := range inputs {
		for max := 0; max <= 40; max++ {
			got := Truncate(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("Truncate(%q, %d) emitted invalid UTF-8: %q", in, max, got)
			}
			// Idempotent under re-application: history compaction re-clips
			// its own output and must get identical bytes back.
			if again := Truncate(got, max); again != got {
				t.Fatalf("Truncate(%q, %d) not idempotent: %q -> %q", in, max, got, again)
			}
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Acme Corp":     "acme-corp",
		" Hello_World ": "hello-world",
		"a@b.com":       "ab-com", // '@' dropped, '.' -> '-'
		"---":           "",
		"Ünïcode Café":  "ncode-caf",
	}
	for in, want := range cases {
		if got := Slug(in, 63); got != want {
			t.Errorf("Slug(%q, 63) = %q; want %q", in, got, want)
		}
	}
	long := Slug("aaaa aaaa aaaa aaaa aaaa aaaa aaaa aaaa aaaa aaaa aaaa aaaa aaaa", 63)
	if len(long) > 63 {
		t.Errorf("Slug cap: got len %d, want <= 63", len(long))
	}
	if got := Slug("abc", 0); got != "abc" {
		t.Errorf("Slug with no cap = %q; want %q", got, "abc")
	}
}
