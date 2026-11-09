package analysis

import "testing"

func TestRewriteRelativeSpecifiersSkipsStringsAndComments(t *testing.T) {
	body := "import { a } from './util.js';\n" +
		"const msg = \"cannot import data from './util.js'\";\n" +
		"// import x from './util.js'\n"
	byName := map[string]string{"util": "js/util.js"}
	out, rw := RewriteRelativeSpecifiers("js/app.js", body, byName)
	want := "import { a } from 'util';\n" +
		"const msg = \"cannot import data from './util.js'\";\n" +
		"// import x from './util.js'\n"
	if out != want || len(rw) != 1 {
		t.Fatalf("got %q (%d rewrites), want %q", out, len(rw), want)
	}
}
