package engine

// Registry-wide sweep for the array-arg contradiction: a description that
// promises a list of strings beside a wire schema advertising object items.
// When they disagree the schema wins - it is a constraint, the prose only
// advice. Any arg that must tolerate loose model output is typed []any, which
// reflects to element kind Interface, rendered as {"type":"object"}; the fix is
// the `items:"..."` struct tag, and this test is the ratchet that keeps a new
// arg from skipping it. The tag's own mechanics are covered by
// tools/itemsschema_test.go - that package cannot run this sweep, because it is
// the one every tool subpackage imports.

import (
	"regexp"
	"testing"

	"openfabrica/tools"
)

// stringListDesc matches a description that tells the model to send strings. Deliberately
// narrow, and anchored on the platform nouns these args are actually about (columns,
// tables, permissions, event types) rather than on any phrasing: a false positive here
// fails a build for nobody's benefit.
var stringListDesc = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\barray of (?:name )?strings\b`),
	regexp.MustCompile(`(?i)\b(?:column|table|permission|event[- ]type|mime|option) ?names?\b`),
	regexp.MustCompile(`(?i)\bcolumns? to (?:include|expose|drop|concatenate)\b`),
}

func TestStringListArgsDoNotAdvertiseObjectItems(t *testing.T) {
	list := tools.DefaultRegistry().List()
	if len(list) < 20 {
		t.Fatalf("registry looks unpopulated (%d tools) - are the tool subpackages imported?", len(list))
	}
	// Harness self-check: the sweep is worthless if no array arg reaches it at all.
	arrayArgs := 0
	for _, td := range list {
		for _, p := range td.Params {
			if p.Type != "array" {
				continue
			}
			arrayArgs++
			if p.Items == nil {
				continue
			}
			if itemType, _ := p.Items["type"].(string); itemType != "object" {
				continue
			}
			for _, re := range stringListDesc {
				if !re.MatchString(p.Description) {
					continue
				}
				t.Errorf("%s arg %q: the description promises a list of strings (%q) but the schema "+
					"advertises items {\"type\":\"object\"}, and the model follows the schema. Tag the "+
					"field `items:\"string\"` (see parseItemsTag in tools/reflect.go).",
					td.Name, p.Name, re.FindString(p.Description))
				break
			}
		}
	}
	if arrayArgs < 20 {
		t.Fatalf("only %d array args reached the sweep; the registry walk is broken", arrayArgs)
	}
}
