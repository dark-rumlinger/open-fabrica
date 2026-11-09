package automation

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// nativeActionTypes is the closed set validateNativeAction accepts. Every place
// that enumerates native_action types for the model must list all of them, so a
// type advertised in one spot is never missing from another (the drift that once
// left run_function off the cadence-reject message). Keep in sync with the
// validateNativeAction switch when adding a type.
var nativeActionTypes = []string{"delete_stale", "count_rows", "truncate", "sql", "trigger_event", "run_function"}

// TestNativeActionEnumerationsListEveryType asserts each accepted type validates
// and that every model-facing enumeration of the set names all of them.
func TestNativeActionEnumerationsListEveryType(t *testing.T) {
	// A minimal valid payload per type, so the set above tracks what the switch
	// actually accepts (a type dropped from the switch fails here).
	valid := map[string]string{
		"delete_stale":  `{"type":"delete_stale","table":"t","age":"7d"}`,
		"count_rows":    `{"type":"count_rows","table":"t"}`,
		"truncate":      `{"type":"truncate","table":"t"}`,
		"sql":           `{"type":"sql","sql":"SELECT 1"}`,
		"trigger_event": `{"type":"trigger_event","event_type":"x"}`,
		"run_function":  `{"type":"run_function","endpoint_path":"p"}`,
	}
	for _, ty := range nativeActionTypes {
		payload, ok := valid[ty]
		if !ok {
			t.Fatalf("no valid payload for %q - add one when introducing a type", ty)
		}
		if err := validateNativeAction(payload); err != nil {
			t.Fatalf("validateNativeAction rejected valid %q: %v", ty, err)
		}
	}

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	sources := map[string]string{
		"type-required error": errText(validateNativeAction(`{"type":""}`)),
		"unknown-type error":  errText(validateNativeAction(`{"type":"nope"}`)),
		"over-cadence reject": guardLLMCadence("a periodic digest", "", "*/1 * * * *", 0, now),
		"native_action arg":   nativeActionArgDesc(t),
	}
	for name, text := range sources {
		if text == "" {
			t.Fatalf("%s produced no text to check", name)
		}
		for _, ty := range nativeActionTypes {
			if !strings.Contains(text, ty) {
				t.Fatalf("%s omits native_action type %q; every enumeration must list all types\ntext: %s", name, ty, text)
			}
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// nativeActionArgDesc reads the ScheduleCreate.native_action arg description.
func nativeActionArgDesc(t *testing.T) string {
	t.Helper()
	f, ok := reflect.TypeOf(ScheduleCreate{}).FieldByName("NativeAction")
	if !ok {
		t.Fatal("ScheduleCreate has no NativeAction field")
	}
	return f.Tag.Get("desc")
}
