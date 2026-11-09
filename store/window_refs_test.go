package store

import "testing"

// WindowGlobalRefs is the single home for the `window.<name>` scan shared by the
// load-order gate, the layout late-globals gate, and the cross-module bridge gate
// (tools/content). This pins its contract so the three consumers stay in sync.
func TestWindowGlobalRefs(t *testing.T) {
	body := `window.WA = {};
window.WA.dock();
const k = window.__gameKeys;
foo(window.location);`
	all, producers := WindowGlobalRefs(body)

	// Every window.<name> referenced, dot form, top-level property only.
	for _, name := range []string{"WA", "__gameKeys", "location"} {
		if !all[name] {
			t.Errorf("all should include %q; got %v", name, all)
		}
	}
	// Only the assigned name is a producer.
	if !producers["WA"] || len(producers) != 1 {
		t.Errorf("producers should be exactly {WA}; got %v", producers)
	}
	// Consumers = all - producers (what the callers derive).
	if producers["__gameKeys"] || producers["location"] {
		t.Errorf("read-only names must not be producers; got %v", producers)
	}
}
