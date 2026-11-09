package agentrt

import (
	"strings"
	"testing"
)

func TestToolNameForFunctionStaysWithinProviderLimit(t *testing.T) {
	short := ToolNameForFunction("orders/create")
	if short != "fn_orders_create" {
		t.Errorf("short path name = %q, want fn_orders_create", short)
	}

	prefix := strings.Repeat("a", 70)
	a := ToolNameForFunction(prefix + "/one")
	b := ToolNameForFunction(prefix + "/two")
	for _, n := range []string{a, b} {
		if len(n) > maxToolNameLen {
			t.Errorf("name %q is %d bytes, over the %d limit", n, len(n), maxToolNameLen)
		}
	}
	if a == b {
		t.Error("two long paths sharing a prefix must still get distinct names")
	}
	if a != ToolNameForFunction(prefix+"/one") {
		t.Error("the name must be deterministic")
	}
}
