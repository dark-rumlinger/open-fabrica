package api

import (
	"fmt"
	"strings"
	"testing"

	"openfabrica/tools"
)

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
