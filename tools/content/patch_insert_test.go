package content

import (
	"strings"
	"testing"
)

func applyInsert(t *testing.T, body string, hunks []map[string]any) string {
	t.Helper()
	if err := expandInsertHunks(body, hunks); err != nil {
		t.Fatalf("expand: %v", err)
	}
	var patches []SearchReplacePatch
	if err := decodeJSONArrayArg(hunks, "patches", &patches); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, pr := ApplyPatches(body, patches)
	if len(pr.NotFound) > 0 {
		t.Fatalf("not found: %v", pr.NotFound)
	}
	return out
}

func TestInsertHunks(t *testing.T) {
	body := "a\nb\nc\n"
	for name, c := range map[string]struct {
		hunk map[string]any
		want string
	}{
		"after":   {map[string]any{"insert_after": "b", "text": "x"}, "a\nb\nx\nc\n"},
		"before":  {map[string]any{"insert_before": "b", "text": "x"}, "a\nx\nb\nc\n"},
		"append":  {map[string]any{"append": "z"}, "a\nb\nc\nz\n"},
		"prepend": {map[string]any{"prepend": "z"}, "z\na\nb\nc\n"},
	} {
		if got := applyInsert(t, body, []map[string]any{c.hunk}); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}

// An edge whose last line repeats elsewhere still appends at the very end.
func TestInsertAppendOnRepeatedLastLine(t *testing.T) {
	body := "}\nfoo()\n}\n"
	got := applyInsert(t, body, []map[string]any{{"append": "end()"}})
	if !strings.HasSuffix(got, "foo()\n}\nend()\n") {
		t.Fatalf("got %q", got)
	}
}

// Inserts keep the anchor line's indentation, and a mid-line anchor inserts on its own lines.
func TestInsertKeepsIndentAndWholeLines(t *testing.T) {
	body := "function f() {\n    return x;\n}\nreload(); foo(); baz();\n"
	got := applyInsert(t, body, []map[string]any{{"insert_before": "return x;", "text": "log();"}})
	if want := "function f() {\n    log();\n    return x;\n}\nreload(); foo(); baz();\n"; got != want {
		t.Fatalf("insert_before: got %q want %q", got, want)
	}
	got = applyInsert(t, body, []map[string]any{{"insert_after": "foo();", "text": "next();"}})
	if want := "function f() {\n    return x;\n}\nreload(); foo(); baz();\nnext();\n"; got != want {
		t.Fatalf("insert_after: got %q want %q", got, want)
	}
}

// append and prepend act on the body as earlier hunks left it, and work on an empty body.
func TestInsertEdgeAfterEarlierHunkAndEmptyBody(t *testing.T) {
	got := applyInsert(t, "a\nlast\n", []map[string]any{{"search": "last", "replace": "LAST"}, {"append": "more"}})
	if want := "a\nLAST\nmore\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := applyInsert(t, "", []map[string]any{{"append": "first"}}); got != "first\n" {
		t.Fatalf("empty body: got %q", got)
	}
}

func TestInsertHunkNeedsText(t *testing.T) {
	if err := expandInsertHunks("a\n", []map[string]any{{"insert_after": "a"}}); err == nil {
		t.Fatal("an insert with no text is refused")
	}
}

// An anchor nothing in the file resembles still gets the shape of the file back.
func TestNearestRegionsFallsBackToOutline(t *testing.T) {
	body := strings.Repeat("alpha beta gamma\n", 50)
	got := nearestRegions(body, []string{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"})
	if len(got) != 1 || got[0]["file_lines"] == nil || got[0]["first_lines"] == "" {
		t.Fatalf("want an outline fallback, got %+v", got)
	}
}
