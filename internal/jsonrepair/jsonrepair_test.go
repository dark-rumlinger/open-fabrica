package jsonrepair

import (
	"encoding/json"
	"testing"
)

// parses reports whether the repaired form of raw decodes as JSON.
func parses(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(Repair(raw)), &out); err != nil {
		t.Fatalf("Repair output did not parse: %v\n raw: %q\n got: %q", err, raw, Repair(raw))
	}
	return out
}

func TestRepair_PythonLiterals(t *testing.T) {
	got := parses(t, `{"a": True, "b": False, "c": None}`)
	if got["a"] != true || got["b"] != false || got["c"] != nil {
		t.Errorf("Python literals mis-normalized: %+v", got)
	}
}

func TestRepair_PythonLiteralsInNestedAndArrays(t *testing.T) {
	got := parses(t, `{"flags":[True,False,None],"obj":{"on":True}}`)
	arr, _ := got["flags"].([]any)
	if len(arr) != 3 || arr[0] != true || arr[1] != false || arr[2] != nil {
		t.Errorf("array of literals mis-normalized: %+v", got["flags"])
	}
	obj, _ := got["obj"].(map[string]any)
	if obj["on"] != true {
		t.Errorf("nested literal mis-normalized: %+v", got["obj"])
	}
}

// A literal substring inside a string value must be preserved verbatim - the
// normalizer only touches tokens OUTSIDE string literals.
func TestRepair_LiteralWordInsideStringIsUntouched(t *testing.T) {
	got := parses(t, `{"note":"The answer is True, or None of them.","ok":True}`)
	if got["note"] != "The answer is True, or None of them." {
		t.Errorf("string value corrupted: %q", got["note"])
	}
	if got["ok"] != true {
		t.Errorf("value literal not normalized: %+v", got["ok"])
	}
}

// Identifiers that merely START with T/F/N (or contain them) must not be
// rewritten - only the exact words.
func TestRepair_PartialWordsNotRewritten(t *testing.T) {
	// `Truthy` / `Falsey` are not valid bare JSON tokens, but the normalizer must
	// not partially rewrite them into `truethy` etc. Wrap them in strings so the
	// document is otherwise valid and we can assert the value survived.
	got := parses(t, `{"x":"Truthy","y":"Nonexistent"}`)
	if got["x"] != "Truthy" || got["y"] != "Nonexistent" {
		t.Errorf("partial words corrupted: %+v", got)
	}
}

func TestRepair_TrailingCommas(t *testing.T) {
	got := parses(t, `{"a":[1,2,3,],"b":{"x":1,},}`)
	arr, _ := got["a"].([]any)
	if len(arr) != 3 {
		t.Errorf("trailing comma in array not handled: %+v", got["a"])
	}
	obj, _ := got["b"].(map[string]any)
	if len(obj) != 1 {
		t.Errorf("trailing comma in object not handled: %+v", got["b"])
	}
}

// A comma inside a string value (even right before a brace char inside the
// string) must be preserved.
func TestRepair_CommaInsideStringPreserved(t *testing.T) {
	got := parses(t, `{"s":"a,}","t":"b,]"}`)
	if got["s"] != "a,}" || got["t"] != "b,]" {
		t.Errorf("comma inside string corrupted: %+v", got)
	}
}

func TestRepair_CombinedArtifacts(t *testing.T) {
	raw := "```json\n{\"goal\":\"line one\nline two\",\"enabled\":True,\"items\":[1,2,],}\n```"
	got := parses(t, raw)
	if got["enabled"] != true {
		t.Errorf("combined repair lost literal: %+v", got)
	}
	if got["goal"] != "line one\nline two" {
		t.Errorf("combined repair lost control-char string: %q", got["goal"])
	}
	items, _ := got["items"].([]any)
	if len(items) != 2 {
		t.Errorf("combined repair lost trailing-comma fix: %+v", got["items"])
	}
}

func TestStripCodeFence_MatchedPair(t *testing.T) {
	if got := StripCodeFence("```json\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("matched fence not stripped: %q", got)
	}
	if got := StripCodeFence("```\n{\"a\":1}\n```\n"); got != `{"a":1}` {
		t.Errorf("bare fence not stripped: %q", got)
	}
}

func TestStripCodeFence_Passthrough(t *testing.T) {
	in := `{"goal":"x"}`
	if got := StripCodeFence(in); got != in {
		t.Errorf("unfenced input should pass through, got %q", got)
	}
}

// A half-fenced (truncated) payload has no closing fence: leave it alone so a
// detector upstream still sees it as not-clean-JSON rather than salvaging junk.
func TestStripCodeFence_UnmatchedLeftAlone(t *testing.T) {
	in := "```json\n{\"a\":1}"
	if got := StripCodeFence(in); got != in {
		t.Errorf("unmatched fence should be left alone, got %q", got)
	}
}

// UnwrapJSON must recover a fenced block buried in narration prose - the shape a
// reasoning model emits when it thinks out loud before/after the JSON it was
// asked to output alone. This is the case StripCodeFence deliberately misses
// (the payload does not START with a fence).
func TestUnwrapJSON_FencedBlockInProse(t *testing.T) {
	in := "Looking at the brief, I need to be careful.\n\nHere's my plan:\n\n" +
		"```json\n{\"goal\":\"x\"}\n```\n\n**Screen inventory check:** all good."
	if got := UnwrapJSON(in); got != `{"goal":"x"}` {
		t.Errorf("prose-wrapped fenced JSON not recovered: %q", got)
	}
}

func TestUnwrapJSON_Passthrough(t *testing.T) {
	// A whole-payload fence, a bare object, and a truncated half-fence each come
	// back exactly as StripCodeFence would leave them (no salvaging of junk).
	cases := map[string]string{
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		`{"a":1}`:                 `{"a":1}`,
		"```json\n{\"a\":1}":      "```json\n{\"a\":1}", // truncated: left alone
	}
	for in, want := range cases {
		if got := UnwrapJSON(in); got != want {
			t.Errorf("UnwrapJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

// Prose with NO fenced block stays prose, so the PLAN first-byte guard still
// rejects it into the corrective-retry path instead of being masked.
func TestUnwrapJSON_ProseWithoutFenceUntouched(t *testing.T) {
	in := "I could not build a plan because the brief is empty."
	if got := UnwrapJSON(in); got != in {
		t.Errorf("fence-less prose should be untouched, got %q", got)
	}
}

func TestEscapeRawControlChars_NoOpOnCleanJSON(t *testing.T) {
	in := "{\n  \"goal\": \"line one\\nline two\",\n  \"tasks\": []\n}"
	if got := EscapeRawControlChars(in); got != in {
		t.Errorf("clean JSON should be unchanged:\n got %q\nwant %q", got, in)
	}
}

func TestRepair_NoOpOnCleanJSON(t *testing.T) {
	in := `{"goal":"x","n":1,"ok":true,"none":null,"list":[1,2,3]}`
	if got := Repair(in); got != in {
		t.Errorf("clean JSON should pass through byte-identical:\n got %q\nwant %q", got, in)
	}
}

func TestTrimUnbalancedTrailingBraces(t *testing.T) {
	if got := TrimUnbalancedTrailingBraces(`{"a":1}` + "\n\n}"); got != `{"a":1}` {
		t.Errorf("trailing-brace overrun not trimmed: %q", got)
	}
}

// A root array (the single-element tool-call wrapper some providers emit) must
// survive intact - it must NOT be truncated at the first inner `}`.
func TestTrimUnbalancedTrailingBraces_RootArrayPreserved(t *testing.T) {
	in := `[{"a":1},{"b":2}]`
	if got := TrimUnbalancedTrailingBraces(in); got != in {
		t.Errorf("root array truncated: got %q want %q", got, in)
	}
	if got := TrimUnbalancedTrailingBraces(`[{"a":1}]` + "\n]"); got != `[{"a":1}]` {
		t.Errorf("array overrun not trimmed: %q", got)
	}
}
