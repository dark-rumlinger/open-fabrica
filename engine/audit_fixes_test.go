package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/store"
)

// Many files just over the clip floor must not spin the budget-shrink loop forever: a
// clip that cannot get shorter ends it.
func TestSpecAuditSourceTerminatesOnManyMidSizeFiles(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.CloseAll)
	body := strings.Repeat("const x = 1; // filler line\n", 85) // ~2.4 KB each
	for i := 0; i < 150; i++ {
		if _, err := pdb.ExecWrite(`INSERT INTO of_files (kind, path, body, metadata, scope) VALUES ('file', ?, ?, '{}', 'page')`,
			fmt.Sprintf("m%03d.js", i), body); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan string, 1)
	go func() { done <- specAuditSource(pdb.Reader(), "", 50_000) }()
	select {
	case src := <-done:
		if src == "" {
			t.Fatal("expected source text")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("specAuditSource did not terminate")
	}
}

// platform_managed is read from the JSON, not matched as a literal substring.
func TestIsPlatformManagedMeta(t *testing.T) {
	for meta, want := range map[string]bool{
		`{"platform_managed":true}`:       true,
		`{"platform_managed": true}`:      true,
		`{"a":1,"platform_managed":true}`: true,
		`{"platform_managed":false}`:      false,
		`{}`:                              false,
		``:                                false,
		`not json`:                        false,
	} {
		if got := isPlatformManagedMeta(meta); got != want {
			t.Errorf("isPlatformManagedMeta(%q) = %v, want %v", meta, got, want)
		}
	}
}

// Repair progress counts every fixable finding, not the capped list a round works on.
func TestRepairProgressIsUncapped(t *testing.T) {
	mk := func(n int) []Issue {
		out := make([]Issue, n)
		for i := range out {
			out[i] = Issue{Severity: store.SeverityError, Category: "runtime", Deterministic: true, Signature: fmt.Sprint(i)}
		}
		return out
	}
	if !repairProgress(mk(100), mk(70)) {
		t.Error("100 -> 70 findings is progress even though both exceed the per-round cap")
	}
	if repairProgress(mk(10), mk(10)) {
		t.Error("an unchanged count is not progress")
	}
}

func TestIsProjectShapeTopic(t *testing.T) {
	for topic, want := range map[string]bool{
		"project shape":  true,
		"Project Shape":  true,
		"shape":          true,
		"response shape": false,
		"data shape":     false,
		"auth model":     false,
	} {
		if got := isProjectShapeTopic(topic); got != want {
			t.Errorf("isProjectShapeTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}

// A shorthand copied into a task's `tools` becomes the one tool that realizes it.
func TestPlanTaskToolsResolveShorthand(t *testing.T) {
	var spec PlanTaskSpec
	if err := json.Unmarshal([]byte(`{"title":"x","tools":["create_auth","manage_api","create_websocket","create_auth"]}`), &spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"manage_auth", "manage_api", "create_websocket"}
	if strings.Join(spec.Tools, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v (create_websocket spans several tools and stays)", spec.Tools, want)
	}
}
