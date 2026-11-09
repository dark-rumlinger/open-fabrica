package engine

// Drift guard for the trigger-events contract.
//
// The `trigger-events` contract is the canonical catalogue of event names a
// project's manage_triggers can fire on; the names themselves are Go constants
// in events/types.go. A new project-facing constant must also appear in the
// contract, or the build LLM never learns it exists and a trigger on it
// silently never fires. This test parses the constant source and asserts every
// project-facing event name is documented. (Emitting an event as a bare string
// literal instead of a constant would also escape this guard.)

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"openfabrica/contracts"
)

// projectFacingPrefixes are the event families a generated app's triggers fire
// on. Engine/admin internals (engine.*, tool.*, project.*, question.*, secret.*,
// schema.*, scheduled.*, settings.*, provider.*, chat.*) are deliberately NOT
// documented as trigger events.
var projectFacingPrefixes = []string{"data.", "auth.", "payment.", "webhook.", "tenant.", "notification."}

// eventConstRe captures the string value of an `EventX EventType = "x.y"` const.
var eventConstRe = regexp.MustCompile(`EventType\s*=\s*"([a-z][a-z._]*)"`)

func TestTriggerEventsContractCoversProjectFacingEvents(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate events/types.go")
	}
	typesPath := filepath.Join(filepath.Dir(thisFile), "..", "events", "types.go")
	src, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatalf("reading %s: %v", typesPath, err)
	}

	matches := eventConstRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 20 {
		t.Fatalf("parsed only %d event constants from events/types.go - did the source format change?", len(matches))
	}

	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	c, ok := all["trigger-events"]
	if !ok {
		t.Fatal("trigger-events contract not found")
	}
	body := c.Body

	var projectFacing int
	for _, m := range matches {
		name := m[1]
		facing := false
		for _, p := range projectFacingPrefixes {
			if strings.HasPrefix(name, p) {
				facing = true
				break
			}
		}
		if !facing {
			continue
		}
		projectFacing++
		if !strings.Contains(body, name) {
			t.Errorf("trigger-events contract omits project-facing event %q (a new events/types.go constant must be documented there)", name)
		}
	}
	if projectFacing < 8 {
		t.Fatalf("found only %d project-facing event constants - the prefix filter may be wrong", projectFacing)
	}
}

// TestTriggerToolListsProjectFacingEvents pins the manage_triggers event_type
// description to the same constants: it is the list the model reads at the call, so
// an event missing there is one the model never wires even though the contract has it.
func TestTriggerToolListsProjectFacingEvents(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..")
	src, err := os.ReadFile(filepath.Join(root, "events", "types.go"))
	if err != nil {
		t.Fatalf("reading events/types.go: %v", err)
	}
	tool, err := os.ReadFile(filepath.Join(root, "tools", "automation", "manage_triggers.go"))
	if err != nil {
		t.Fatalf("reading manage_triggers.go: %v", err)
	}
	descRe := regexp.MustCompile(`arg:"event_type,required"\s+desc:"([^"]*)"`)
	m := descRe.FindSubmatch(tool)
	if m == nil {
		t.Fatal("event_type description not found in manage_triggers.go")
	}
	desc := string(m[1])
	for _, c := range eventConstRe.FindAllStringSubmatch(string(src), -1) {
		name := c[1]
		for _, p := range projectFacingPrefixes {
			if strings.HasPrefix(name, p) && !strings.Contains(desc, name) {
				t.Errorf("manage_triggers event_type description omits project-facing event %q", name)
			}
		}
	}
}
