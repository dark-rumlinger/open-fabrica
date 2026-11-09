package content

import (
	"strings"
	"testing"
)

// This file is the canonical decision table for the patch matcher. One row per
// path through locatePatch (the 7 locate layers) and ApplyPatches (the terminal
// outcome buckets), so the whole contract is legible in one place and a change
// to the layer order, a note string, or a bucket rule fails here with the row
// named. The focused tests in artifacts_patch_layered_test.go and
// patch_preflight_test.go stay as the per-behavior "why" cases; this is the
// one spec that enumerates every row.
//
// Locate layers, in the order locatePatch tries them, each landing in Applied:
//
//	L1  exact                 note ""                         (quiet)
//	L1b literal \n \t \r       note "interpreted literal ..."
//	L2  CRLF -> LF             note "line endings normalized"
//	L3  trailing whitespace    note "ignoring trailing whitespace"
//	L4  indentation flexible   note "indentation adjusted to fit"
//	L4b collapsed, unique      note "whitespace normalized ..."
//	L5  fuzzy (>=0.90, +0.05)  note "fuzzy match ~N% ..."
//
// Terminal buckets and the condition that routes a hunk to each:
//
//	Applied         a layer located it and search != replace (or append/prepend)
//	NotFound        no layer located it and the replace text is not already present
//	AlreadyApplied  no layer located it but the replace text (>= 20 trimmed) is present
//	NoOp            search == replace and the anchor is locatable (non-fuzzy)
//	Unanchored      search == replace and the anchor is not locatable
//	Refused         a blank replace located only by the fuzzy layer (content-loss guard)
//	Chained         the target span lies inside text an earlier hunk in the batch wrote

type dtExpect struct {
	applied, notFound, noOp, refused, chained, alreadyApplied, unanchored int
}

func (e dtExpect) check(t *testing.T, name string, r PatchResult) {
	t.Helper()
	got := dtExpect{
		applied:        len(r.Applied),
		notFound:       len(r.NotFound),
		noOp:           len(r.NoOp),
		refused:        len(r.Refused),
		chained:        len(r.Chained),
		alreadyApplied: len(r.AlreadyApplied),
		unanchored:     len(r.Unanchored),
	}
	if got != e {
		t.Fatalf("%s: bucket counts\n got  %+v\n want %+v\n (Applied=%v NotFound=%v NoOp=%v Refused=%v Chained=%v AlreadyApplied=%v Unanchored=%v)",
			name, got, e, r.Applied, r.NotFound, r.NoOp, r.Refused, r.Chained, r.AlreadyApplied, r.Unanchored)
	}
}

func TestPatchDecisionTable(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		patches  []SearchReplacePatch
		wantBody string
		// noteSub must appear in the joined Warnings; "" means Warnings must be empty.
		noteSub string
		want    dtExpect
	}{
		// --- locate layers, each resolves to Applied ---
		{
			name:     "L1_exact",
			body:     "alpha\nbeta\ngamma\n",
			patches:  []SearchReplacePatch{{Search: "beta", Replace: "BETA"}},
			wantBody: "alpha\nBETA\ngamma\n",
			noteSub:  "",
			want:     dtExpect{applied: 1},
		},
		{
			name:     "L1b_literal_escape",
			body:     "const a = 1;\nconst b = 2;\nconst c = 3;\n",
			patches:  []SearchReplacePatch{{Search: `const b = 2;\nconst c = 3;`, Replace: `const b = 2;\nconst c = 3;\nconst d = 4;`}},
			wantBody: "const a = 1;\nconst b = 2;\nconst c = 3;\nconst d = 4;\n",
			noteSub:  "interpreted literal",
			want:     dtExpect{applied: 1},
		},
		{
			name:     "L2_crlf_to_lf",
			body:     "line one\nline two\nline three\n",
			patches:  []SearchReplacePatch{{Search: "line one\r\nline two", Replace: "line one\r\nLINE TWO"}},
			wantBody: "line one\nLINE TWO\nline three\n",
			noteSub:  "line endings normalized",
			want:     dtExpect{applied: 1},
		},
		{
			name:     "L3_trailing_whitespace",
			body:     "if (ok) {\n    doThing();\n}\n",
			patches:  []SearchReplacePatch{{Search: "if (ok) {   \n    doThing();", Replace: "if (ok) {\n    doBetterThing();"}},
			wantBody: "if (ok) {\n    doBetterThing();\n}\n",
			noteSub:  "ignoring trailing whitespace",
			want:     dtExpect{applied: 1},
		},
		{
			name:     "L4_indentation_flexible",
			body:     "function f() {\n        const a = 1;\n        const b = 2;\n}\n",
			patches:  []SearchReplacePatch{{Search: "const a = 1;\nconst b = 2;", Replace: "const a = 10;\nconst b = 20;"}},
			wantBody: "function f() {\n        const a = 10;\n        const b = 20;\n}\n",
			noteSub:  "indentation adjusted to fit",
			want:     dtExpect{applied: 1},
		},
		{
			name:     "L4b_collapsed_unique_line_count_drift",
			body:     "/* Speech-bubble toast */\n.toast-bubble {\n  position: fixed;\n  bottom: 22px;\n}\n",
			patches:  []SearchReplacePatch{{Search: ".toast-bubble { position: fixed;", Replace: ".toast-bubble {\n  position: absolute;"}},
			wantBody: "/* Speech-bubble toast */\n.toast-bubble {\n  position: absolute;\n  bottom: 22px;\n}\n",
			noteSub:  "whitespace normalized",
			want:     dtExpect{applied: 1},
		},
		{
			name:     "L5_fuzzy",
			body:     "const greeting = computeGreeting(userName);\n",
			patches:  []SearchReplacePatch{{Search: "const greeting = computeGreeting(username);", Replace: "const greeting = computeGreeting(displayName);"}},
			wantBody: "const greeting = computeGreeting(displayName);\n",
			noteSub:  "fuzzy match",
			want:     dtExpect{applied: 1},
		},

		// --- terminal buckets ---
		{
			name:     "NotFound_distant_anchor",
			body:     "const greeting = computeGreeting(userName);\n",
			patches:  []SearchReplacePatch{{Search: "return databaseConnection.query(sqlStatement);", Replace: "whatever"}},
			wantBody: "const greeting = computeGreeting(userName);\n",
			noteSub:  "",
			want:     dtExpect{notFound: 1},
		},
		{
			name:     "NoOp_search_equals_replace_locatable",
			body:     "alpha\nbeta\ngamma\n",
			patches:  []SearchReplacePatch{{Search: "beta", Replace: "beta"}},
			wantBody: "alpha\nbeta\ngamma\n",
			noteSub:  "",
			want:     dtExpect{noOp: 1},
		},
		{
			name:     "Unanchored_search_equals_replace_absent",
			body:     "alpha\nbeta\ngamma\n",
			patches:  []SearchReplacePatch{{Search: "zzz_absent_anchor_xyz", Replace: "zzz_absent_anchor_xyz"}},
			wantBody: "alpha\nbeta\ngamma\n",
			noteSub:  "",
			want:     dtExpect{unanchored: 1},
		},
		{
			name:     "Refused_fuzzy_empty_delete",
			body:     "function compute(a, b) {\n  const total = a + b;\n  return total;\n}\n",
			patches:  []SearchReplacePatch{{Search: "function compute(a, b) {\n  const total = a + c;\n  return total;\n}", Replace: ""}},
			wantBody: "function compute(a, b) {\n  const total = a + b;\n  return total;\n}\n",
			noteSub:  "fuzzy",
			want:     dtExpect{refused: 1},
		},
		{
			name:     "AlreadyApplied_idempotent_hunk",
			body:     "  return formattedTitle + name;\n",
			patches:  []SearchReplacePatch{{Search: "  return name;", Replace: "  return formattedTitle + name;"}},
			wantBody: "  return formattedTitle + name;\n",
			noteSub:  "",
			want:     dtExpect{alreadyApplied: 1},
		},
		{
			name: "Chained_second_hunk_in_first_output",
			body: "const a = 1;\n",
			patches: []SearchReplacePatch{
				{Search: "const a = 1;", Replace: "const a = 2;"},
				{Search: "const a = 2;", Replace: "const a = 3;"},
			},
			wantBody: "const a = 2;\n",
			noteSub:  "",
			want:     dtExpect{applied: 1, chained: 1},
		},
		{
			name:     "EdgeInsert_append",
			body:     "line1\n",
			patches:  []SearchReplacePatch{{Append: "appended"}},
			wantBody: "line1\nappended\n",
			noteSub:  "",
			want:     dtExpect{applied: 1},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, res := ApplyPatches(c.body, c.patches)
			if out != c.wantBody {
				t.Fatalf("%s: body\n got  %q\n want %q", c.name, out, c.wantBody)
			}
			c.want.check(t, c.name, res)
			warns := strings.Join(res.Warnings, " ")
			if c.noteSub == "" {
				if len(res.Warnings) != 0 {
					t.Fatalf("%s: expected no warnings, got %v", c.name, res.Warnings)
				}
			} else if !strings.Contains(warns, c.noteSub) {
				t.Fatalf("%s: warnings missing %q, got %v", c.name, c.noteSub, res.Warnings)
			}
		})
	}
}
