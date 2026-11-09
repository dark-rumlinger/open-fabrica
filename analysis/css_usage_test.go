package analysis

import (
	"reflect"
	"testing"
)

func TestClassGroupsUsed(t *testing.T) {
	// Each class= occurrence is one group, so a caller can tell a hook class riding
	// with a styling class (act + a-like) from a truly solo class.
	body := `'<button class="act a-like' + (liked?' liked':'') + '">' + '<span class="c">'`
	got := ClassGroupsUsed(body)
	want := [][]string{{"act", "a-like"}, {"c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ClassGroupsUsed = %v, want %v", got, want)
	}
}

// classTokens flattens ClassGroupsUsed into a deduplicated, source-ordered list,
// so the extraction cases below can be stated as plain token lists.
func classTokens(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range ClassGroupsUsed(body) {
		for _, tok := range g {
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	return out
}

func TestClassGroupsUsedTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "static html",
			body: `<button class="pill-btn submit">Go</button>`,
			want: []string{"pill-btn", "submit"},
		},
		{
			name: "js template with interpolation keeps only static prefix",
			body: "el.innerHTML = `<span class=\"act ${liked ? 'liked' : ''}\">`;",
			want: []string{"act"},
		},
		{
			name: "js string concatenation stops at the closing quote",
			body: `'<a class="nav-item' + (on ? ' active' : '') + '" href="/x">'`,
			want: []string{"nav-item"},
		},
		{
			name: "className assignment via DOM property",
			body: `b.className = 'avatar sm';`,
			want: []string{"avatar", "sm"},
		},
		{
			name: "data-class look-alike is not a class usage",
			body: `<div data-class="not-a-style">`,
			want: nil,
		},
		{
			name: "dedup across occurrences, source order",
			body: `<i class="act"></i><i class="act reply"></i>`,
			want: []string{"act", "reply"},
		},
		{
			name: "fully dynamic class yields nothing",
			body: "`<div class=\"${cls}\">`",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classTokens(c.body)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("classTokens(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}
