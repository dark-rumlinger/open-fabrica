package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TruncateResultAt caps a body and names the original size; an under-cap body
// passes through byte-identical.
func TestTruncateResultAt(t *testing.T) {
	long := strings.Repeat("x", 100)
	out := TruncateResultAt(long, 50)
	if !strings.HasPrefix(out, strings.Repeat("x", 50)) {
		t.Errorf("truncated body should keep the first 50 bytes")
	}
	if !strings.Contains(out, "[truncated: showed first ~50 of 100 bytes") {
		t.Errorf("missing truncation note, got %q", out)
	}
	if TruncateResultAt("short", 50) != "short" {
		t.Error("under-cap body must pass through unchanged")
	}
}

// TruncateResult falls to the shared default cap when the tool declares no
// MaxResult (or the registry is absent). Chat and the engine both route
// through this, so the caps cannot drift.
func TestTruncateResultDefaultCap(t *testing.T) {
	long := strings.Repeat("x", 5000)
	out := TruncateResult(nil, "whatever", long)
	if !strings.Contains(out, "[truncated: showed first ~3000 of 5000 bytes") {
		t.Errorf("default cap not applied, got tail %q", out[len(out)-80:])
	}
}

// A directive in a result's data (`stop`, a hint) must survive a tail truncation. Map
// keys marshal alphabetically, which put `stop` after a bulky `failures` list and cut it
// off first; MarshalToolResult now leads with those fields.
func TestMarshalToolResultKeepsDirectivesAheadOfBulk(t *testing.T) {
	bulk := make([]string, 50)
	for i := range bulk {
		bulk[i] = strings.Repeat("x", 100)
	}
	out, err := MarshalToolResult(Ok(map[string]any{
		"failures": bulk, "stop": "STOP: do not replay again", "hint": "read the failure", "zeta": 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	cut := TruncateResultAt(string(out), 400)
	if !strings.Contains(cut, "STOP: do not replay again") || !strings.Contains(cut, "read the failure") {
		t.Fatalf("a directive was truncated away:\n%s", cut)
	}
	if strings.Index(string(out), `"stop"`) > strings.Index(string(out), `"failures"`) {
		t.Fatal("stop must be written before the bulky fields")
	}
	var back struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out, &back); err != nil || back.Data["zeta"] != float64(1) {
		t.Fatalf("result must stay valid JSON with every key: err=%v data=%v", err, back.Data)
	}
}
