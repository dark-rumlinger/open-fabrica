package content

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// manage_files must never create a second of_test.js the flow runner ignores (projects
// 8, 9, 16): writes are refused toward write_flows, and a get serves the real script.
func TestManageFiles_FlowScriptRoutesToManageTesting(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	src := `export default { flows: [{ name: "a", steps: [{ goto: "/" }] }] }`
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, storage, body, scope) VALUES ('test', 'of_test.js', 'inline', ?, 'test')`, src); err != nil {
		t.Fatalf("seed flow script: %v", err)
	}

	for _, p := range []string{"of_test.js", "/files/of_test.js", "js/of_test.js"} {
		res, err := (&ArtSave{Kind: "file", FilePath: p, Body: src}).Run(ctx)
		if err != nil || res == nil || res.Success || res.Code != "flow_script" || !strings.Contains(res.Error, "write_flows") {
			t.Fatalf("save %q must be refused toward write_flows, got %+v (err %v)", p, res, err)
		}
	}
	res, err := (&ArtPatch{FilePath: "/files/of_test.js", Patches: []map[string]any{{"search": "a", "replace": "b"}}}).Run(ctx)
	if err != nil || res == nil || res.Success || res.Code != "flow_script" {
		t.Fatalf("patch must be refused toward write_flows, got %+v (err %v)", res, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_files WHERE path = 'of_test.js'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("no shadow copy may be created, rows=%d err=%v", n, err)
	}

	got, err := (&ArtGet{FilePath: "of_test.js"}).Run(ctx)
	if err != nil || got == nil || !got.Success {
		t.Fatalf("get must serve the flow script, got %+v (err %v)", got, err)
	}
	if data, _ := got.Data.(map[string]any); data["body"] != src {
		t.Errorf("get must return the stored script, got %+v", got.Data)
	}
}

// The test script never ships as a page asset: attaching a stray copy of it (project
// 16's fix-pass did) is refused.
func TestManageFiles_FlowScriptNeverAttached(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if _, rej := linkFileToPage(ctx, "of_test.js", "/", 0); rej == nil || rej.Code != "flow_script" {
		t.Fatalf("attaching of_test.js must be refused, got %+v", rej)
	}
}

// With the script's owner loaded, a manage_files save or patch of of_test.js goes to it
// instead of costing a rejected turn: the whole script is handed over, a patch is applied
// to the stored script first, and a patch that does not apply changes nothing.
func TestManageFiles_FlowScriptEditsAreRoutedToItsOwner(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	src := `export default { flows: [{ name: "a", steps: [{ click: "text=Add habit" }] }] }`
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, storage, body, scope) VALUES ('test', 'of_test.js', 'inline', ?, 'test')`, src); err != nil {
		t.Fatalf("seed flow script: %v", err)
	}
	var got string
	tools.FlowScriptSaver = func(_ *tools.ToolContext, source string) (*tools.Result, error) {
		got = source
		return tools.Ok(map[string]any{"saved": "of_test.js"}), nil
	}
	t.Cleanup(func() { tools.FlowScriptSaver = nil })

	res, err := (&ArtPatch{FilePath: "/of_test.js", Patches: []map[string]any{{"search": "text=Add habit", "replace": "#btnSave"}}}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("a clean patch must be routed, got %+v (err %v)", res, err)
	}
	if want := strings.Replace(src, "text=Add habit", "#btnSave", 1); got != want {
		t.Errorf("owner received %q, want %q", got, want)
	}
	if data, _ := res.Data.(map[string]any); data["routed"] == nil {
		t.Errorf("the result must say it was routed: %+v", res.Data)
	}

	got = ""
	res, _ = (&ArtPatch{FilePath: "of_test.js", Patches: []map[string]any{{"search": "no such text", "replace": "x"}}}).Run(ctx)
	if res == nil || res.Success || res.Code != "flow_script_patch" || got != "" {
		t.Fatalf("a patch that does not apply must change nothing, got %+v (saved %q)", res, got)
	}

	res, _ = (&ArtSave{Kind: "file", FilePath: "of_test.js", Body: src}).Run(ctx)
	if res == nil || !res.Success || got != src {
		t.Fatalf("a save must be handed to the owner, got %+v (saved %q)", res, got)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_files WHERE path = 'of_test.js'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("no shadow copy may be created, rows=%d err=%v", n, err)
	}
}
