package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"openfabrica/internal/funcsandbox"
	"openfabrica/tools"
)

// manage_function's rate_limit and timeout_ms descriptions are hand-typed prose a
// struct tag cannot compute, so they can drift from what the runtime does. Both
// have: the descriptions read "requests/sec" while the shared limiter counts per
// MINUTE (60x), and quoted a ~5000ms default while funcsandbox.Budget is 30s
// (6x). These guards derive the truth from the runtime constants, mirroring
// TestBodySizeProseMatchesConstants in tools/content.

// functionArgProse returns the LLM-facing descriptions for one arg across both
// the create and update schemas - the two places a figure can drift apart.
func functionArgProse(t *testing.T, field string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, typ := range map[string]reflect.Type{
		"FunctionCreate": reflect.TypeOf(FunctionCreate{}),
		"FunctionUpdate": reflect.TypeOf(FunctionUpdate{}),
	} {
		f, ok := typ.FieldByName(field)
		if !ok {
			t.Fatalf("%s has no %s field", name, field)
		}
		out[name+"."+field+" desc"] = f.Tag.Get("desc")
	}
	return out
}

// TestFunctionTimeoutProseMatchesConstants derives both figures the timeout prose
// quotes: the default (funcsandbox.Budget, the value an omitted timeout_ms
// resolves to) and the ceiling this package enforces.
func TestFunctionTimeoutProseMatchesConstants(t *testing.T) {
	defaultMS := fmt.Sprintf("%d", funcsandbox.Budget.Milliseconds())
	maxMS := fmt.Sprintf("%d", maxFunctionTimeoutMS)

	create := functionArgProse(t, "TimeoutMS")["FunctionCreate.TimeoutMS desc"]
	if !strings.Contains(create, defaultMS) {
		t.Errorf("create timeout prose must quote the real default %sms (funcsandbox.Budget); prose=%q", defaultMS, create)
	}
	for where, prose := range functionArgProse(t, "TimeoutMS") {
		if !strings.Contains(prose, maxMS) {
			t.Errorf("%s: must quote the enforced ceiling %sms (maxFunctionTimeoutMS); prose=%q", where, maxMS, prose)
		}
	}
}

// TestFunctionTimeoutCeilingEnforced pins the clamp itself: without it a handler
// can outlive the server's WriteTimeout, holding a goroutine on a response no
// client is still reading.
func TestFunctionTimeoutCeilingEnforced(t *testing.T) {
	db := setupFunctionDB(t)
	ctx := &tools.ToolContext{DB: db}

	code := `function handler(req, ctx) { return { ok: true }; }`
	res, err := (&FunctionCreate{EndpointPath: "slow", Code: code, TimeoutMS: maxFunctionTimeoutMS + 1}).Run(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Success {
		t.Fatal("a timeout_ms past the ceiling should be rejected")
	}
	if !strings.Contains(res.Error, fmt.Sprintf("%d", maxFunctionTimeoutMS)) {
		t.Errorf("rejection should name the ceiling; got %q", res.Error)
	}

	// The ceiling itself is allowed.
	if res, err := (&FunctionCreate{EndpointPath: "ok", Code: code, TimeoutMS: maxFunctionTimeoutMS}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("timeout_ms at the ceiling should be accepted: err=%v res=%+v", err, res)
	}
}
