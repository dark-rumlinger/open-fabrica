package public

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// hasRail reports whether the node at the given path through a pinned schema
// carries additionalProperties:false. Path segments walk map keys.
func hasRail(t *testing.T, pinned string, path ...string) bool {
	t.Helper()
	var node map[string]any
	if err := json.Unmarshal([]byte(pinned), &node); err != nil {
		t.Fatalf("pinned schema is not JSON: %v\n%s", err, pinned)
	}
	for _, seg := range path {
		next, ok := node[seg].(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object in %s", path, seg, pinned)
		}
		node = next
	}
	ap, set := node["additionalProperties"]
	return set && ap == false
}

// TestPinSchemaRailsMakesAuthoredSchemaStrict covers the shape a generate
// endpoint actually gets authored with: an object with every property required
// and no additionalProperties anywhere. Pinning the rail is what flips it from
// "hint" to a schema a provider can be told to enforce strictly.
func TestPinSchemaRailsMakesAuthoredSchemaStrict(t *testing.T) {
	authored := `{"type":"object","properties":{"html":{"type":"string"},"css":{"type":"string"}},"required":["html","css"]}`
	if schemaIsStrictCompatible(authored) {
		t.Fatal("authored schema was already strict-compatible; the fixture no longer covers the case")
	}

	pinned := pinSchemaRails(authored)
	if !hasRail(t, pinned) {
		t.Errorf("root did not get additionalProperties:false: %s", pinned)
	}
	if !schemaIsStrictCompatible(pinned) {
		t.Errorf("pinned schema is still not strict-compatible: %s", pinned)
	}
	// The rail must not change what the reply has to contain.
	if !structuralSchemaCheck(`{"html":"<p>hi</p>","css":"p{}"}`, pinned) {
		t.Error("a conforming reply failed the shape check against the pinned schema")
	}
	if structuralSchemaCheck(`{"html":"<p>hi</p>"}`, pinned) {
		t.Error("a reply missing a required key passed the shape check")
	}
}

// TestPinSchemaRailsNested walks the keywords that carry a subschema, since a
// rail on the root alone still leaves a nested object free to invent keys.
func TestPinSchemaRailsNested(t *testing.T) {
	authored := `{
		"type":"object",
		"properties":{
			"meta":{"type":"object","properties":{"title":{"type":"string"}}},
			"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}}}}
		},
		"required":["meta","items"]
	}`
	pinned := pinSchemaRails(authored)

	if !hasRail(t, pinned) {
		t.Errorf("root missing rail: %s", pinned)
	}
	if !hasRail(t, pinned, "properties", "meta") {
		t.Errorf("nested object property missing rail: %s", pinned)
	}
	if !hasRail(t, pinned, "properties", "items", "items") {
		t.Errorf("array item schema missing rail: %s", pinned)
	}
}

// TestPinSchemaRailsLeavesIntentAlone locks the two cases the pass must not
// touch: an object that declares no properties is an open map by intent, and an
// explicit additionalProperties is the author's decision either way.
func TestPinSchemaRailsLeavesIntentAlone(t *testing.T) {
	openMap := `{"type":"object","description":"arbitrary key/value pairs"}`
	if got := pinSchemaRails(openMap); strings.Contains(got, "additionalProperties") {
		t.Errorf("open map got a rail it did not ask for: %s", got)
	}

	explicit := `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":true}`
	var node map[string]any
	if err := json.Unmarshal([]byte(pinSchemaRails(explicit)), &node); err != nil {
		t.Fatalf("pinned schema is not JSON: %v", err)
	}
	if node["additionalProperties"] != true {
		t.Errorf("explicit additionalProperties:true was overwritten: %v", node["additionalProperties"])
	}

	subschema := `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"object","properties":{"b":{"type":"string"}}}}`
	if !hasRail(t, pinSchemaRails(subschema), "additionalProperties") {
		t.Error("an additionalProperties subschema did not get its own rail")
	}
}

// TestPinSchemaRailsKeepsOptionalOptional guards against the pass over-reaching
// into `required`: pinning constrains property NAMES, it must never promote an
// optional field to mandatory.
func TestPinSchemaRailsKeepsOptionalOptional(t *testing.T) {
	authored := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`
	pinned := pinSchemaRails(authored)

	var node map[string]any
	if err := json.Unmarshal([]byte(pinned), &node); err != nil {
		t.Fatalf("pinned schema is not JSON: %v", err)
	}
	req, _ := json.Marshal(node["required"])
	if string(req) != `["a"]` {
		t.Errorf("required changed to %s, want [\"a\"]", req)
	}
	if schemaIsStrictCompatible(pinned) {
		t.Error("a schema with an optional property must not be reported strict-compatible")
	}
	if !structuralSchemaCheck(`{"a":"x"}`, pinned) {
		t.Error("a reply omitting the optional property failed the shape check")
	}
}

// TestPinSchemaRailsPassesThroughUnusable keeps the pass conservative: what it
// cannot parse it must hand to the provider exactly as stored.
func TestPinSchemaRailsPassesThroughUnusable(t *testing.T) {
	for _, in := range []string{"", "   ", "not json", `["array","root"]`} {
		if got := pinSchemaRails(in); got != in {
			t.Errorf("pinSchemaRails(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestLogSnippet covers the sample that now rides in the schema-miss log and in
// the corrective prompt: one line, bounded, and never a split rune.
func TestLogSnippet(t *testing.T) {
	if got := logSnippet("hello\n\tworld  again", 100); got != "hello world again" {
		t.Errorf("logSnippet collapsed to %q", got)
	}
	long := strings.Repeat("x", 500)
	got := logSnippet(long, 200)
	if len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Errorf("logSnippet(500 bytes, 200) = %d bytes ending %q", len(got), got[len(got)-3:])
	}
	// A clip that would land mid-rune backs up to a boundary.
	multi := strings.Repeat("é", 100) // 2 bytes per rune
	if got := logSnippet(multi, 25); !utf8.ValidString(strings.TrimSuffix(got, "...")) {
		t.Errorf("logSnippet split a rune: %q", got)
	}
}
