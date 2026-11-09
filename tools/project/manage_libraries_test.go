package project

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupPlanDB is a migrated project database seeded with one run carrying the
// given plan JSON.
func setupPlanDB(t *testing.T, planJSON string) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`, planJSON); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return db
}

func libraryNames(t *testing.T, db dbh.Handle) []string {
	t.Helper()
	var planJSON string
	if err := db.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	var plan struct {
		ExternalLibraries []struct {
			Name string `json:"name"`
		} `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("parse plan: %v", err)
	}
	out := make([]string, 0, len(plan.ExternalLibraries))
	for _, l := range plan.ExternalLibraries {
		out = append(out, l.Name)
	}
	return out
}

// add_library on an already-declared library must be an idempotent no-op that
// reports success (not a hard failure that burns a recovery turn), and must not
// duplicate the entry in the plan.
func TestAddLibrary_IdempotentOnDuplicate(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"Press Start 2P","url":"https://fonts.example/press.css"}]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryAdd{Library: map[string]any{
		"name": "Press Start 2P",
		"url":  "https://fonts.example/press.css",
	}}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("re-adding a declared library must succeed (idempotent), got failure: %+v", res)
	}
	if names := libraryNames(t, db); len(names) != 1 {
		t.Fatalf("duplicate must not be appended; got %v", names)
	}
}

// Re-adding an already-named library that is INCOMPLETE upgrades it in place:
// the missing field (here module_name, which the importmap is keyed on) is filled
// from the new call rather than swallowed as a no-op. This is the model's only way
// to repair a kind="module" lib the plan declared without a specifier.
func TestAddLibrary_UpgradesIncompleteEntry(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"three.js","url":"https://cdn/three.module.js","kind":"module"}]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryAdd{Library: map[string]any{
		"name": "three.js", "url": "https://cdn/three.module.js", "kind": "module", "module_name": "three",
	}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("upgrade run: err=%v res=%+v", err, res)
	}

	var planJSON string
	if err := db.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ExternalLibraries []map[string]any `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.ExternalLibraries) != 1 {
		t.Fatalf("upgrade must not duplicate the entry; got %d", len(raw.ExternalLibraries))
	}
	if raw.ExternalLibraries[0]["module_name"] != "three" {
		t.Errorf("module_name should be filled to \"three\", got %v", raw.ExternalLibraries[0]["module_name"])
	}
}

// Re-adding a declared library with a CHANGED url bumps the version in place
// (e.g. an r128 -> r160 core upgrade): the existing
// entry's url is updated, not left untouched, and the result reports the change
// so the model knows the new bytes are live instead of mistaking a silent no-op
// for an applied update.
func TestAddLibrary_UpdatesChangedURL(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"three.js core","kind":"module","module_name":"three","url":"https://cdnjs.cloudflare.com/ajax/libs/three.js/r128/three.module.js"}]}`)
	ctx := &tools.ToolContext{DB: db}

	const newURL = "https://unpkg.com/three@0.160.0/build/three.module.js"
	res, err := (&LibraryAdd{Library: map[string]any{
		"name": "three.js core", "kind": "module", "module_name": "three", "url": newURL,
	}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("version-bump run: err=%v res=%+v", err, res)
	}
	data, _ := res.Data.(map[string]any)
	if data["updated"] != "three.js core" {
		t.Fatalf("result must report the update, got %+v", data)
	}

	var planJSON string
	if err := db.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ExternalLibraries []map[string]any `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.ExternalLibraries) != 1 {
		t.Fatalf("version bump must update in place, not duplicate; got %d entries", len(raw.ExternalLibraries))
	}
	if got := raw.ExternalLibraries[0]["url"]; got != newURL {
		t.Errorf("url should be updated to r160, got %v", got)
	}
}

// A raw font-file URL (the regression: a gstatic .woff2 that renders as a
// <script> the browser refuses to execute) is rejected at the gate and never
// reaches the plan; the error steers the model to the css2 stylesheet endpoint.
func TestAddLibrary_RejectsFontFileURL(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryAdd{Library: map[string]any{
		"name": "Press Start 2P",
		"url":  "https://fonts.gstatic.com/s/pressstart2p/v15/e3t4euO8T-267oIAQAu6jDQyK3nVivM.woff2",
	}}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("a raw font-file URL must be rejected, got success: %+v", res)
	}
	for _, want := range []string{"css2", "stylesheet"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("rejection should steer to the css2 stylesheet endpoint; error = %q", res.Error)
		}
	}
	if names := libraryNames(t, db); len(names) != 0 {
		t.Fatalf("rejected font file must not be added to the plan; got %v", names)
	}
}

// A genuinely new library is appended and reported.
func TestAddLibrary_AppendsNew(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryAdd{Library: map[string]any{
		"name": "marked",
		"url":  "https://cdn.example/marked.js",
	}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("add new: err=%v res=%+v", err, res)
	}
	names := libraryNames(t, db)
	if len(names) != 1 || names[0] != "marked" {
		t.Fatalf("expected [marked], got %v", names)
	}
}

// remove_library drops exactly the named entry and leaves the rest intact: the fix
// for a library added by mistake or pointing at a nonexistent URL, which add_library
// (upsert-only) can never take back out.
func TestRemoveLibrary_RemovesDeclared(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"marked","url":"https://cdn.example/marked.js"},{"name":"Chart.js","url":"https://cdn.example/chart.js"}]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryRemove{Name: "marked"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("remove: err=%v res=%+v", err, res)
	}
	data, _ := res.Data.(map[string]any)
	if data["removed"] != "marked" {
		t.Fatalf("result must report the removal, got %+v", data)
	}
	names := libraryNames(t, db)
	if len(names) != 1 || names[0] != "Chart.js" {
		t.Fatalf("only 'marked' should be removed; got %v", names)
	}
}

// Removing a name that is not declared fails (rather than silently succeeding, which
// would let a typo'd removal read as done) and lists the declared names so the model
// can retry with the right one - and the plan is left untouched.
func TestRemoveLibrary_NotFoundListsDeclared(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"Chart.js","url":"https://cdn.example/chart.js"}]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryRemove{Name: "marked"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("removing an undeclared library must fail, got success: %+v", res)
	}
	if !strings.Contains(res.Error, "Chart.js") {
		t.Errorf("miss should list the declared names to self-correct; error = %q", res.Error)
	}
	if names := libraryNames(t, db); len(names) != 1 || names[0] != "Chart.js" {
		t.Fatalf("a failed remove must not mutate the plan; got %v", names)
	}
}

// Removing from a plan with no declared libraries fails with a clear "nothing to
// remove" message rather than an opaque miss.
func TestRemoveLibrary_EmptyPlan(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryRemove{Name: "marked"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("remove from empty plan must fail, got success: %+v", res)
	}
	if !strings.Contains(res.Error, "no external libraries") {
		t.Errorf("error should say the plan declares no external libraries; got %q", res.Error)
	}
}

// A blank name is rejected before any DB work.
func TestRemoveLibrary_RequiresName(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"marked","url":"https://cdn.example/marked.js"}]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryRemove{Name: "   "}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("blank name must be rejected, got success: %+v", res)
	}
	if names := libraryNames(t, db); len(names) != 1 {
		t.Fatalf("rejected remove must not mutate the plan; got %v", names)
	}
}

func libraryEntry(t *testing.T, db dbh.Handle, name string) map[string]any {
	t.Helper()
	var planJSON string
	if err := db.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	var plan struct {
		ExternalLibraries []map[string]any `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("parse plan: %v", err)
	}
	for _, l := range plan.ExternalLibraries {
		if l["name"] == name {
			return l
		}
	}
	t.Fatalf("library %q not declared", name)
	return nil
}

// A new url is a different file: a global_name and integrity hash carried over
// from the old one would be wrong, so they are cleared (and reported) unless the
// same call supplies them again.
func TestAddLibrary_URLChangeClearsStaleFileFields(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[{"name":"qr","url":"https://cdn.example/old-qr.min.js","global_name":"OldQR","integrity":"sha384-old"}]}`)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&LibraryAdd{Library: map[string]any{"name": "qr", "url": "https://cdn.example/new-qr.js"}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("run: %v %+v", err, res)
	}
	entry := libraryEntry(t, db, "qr")
	if _, ok := entry["global_name"]; ok {
		t.Errorf("stale global_name must be cleared: %+v", entry)
	}
	if _, ok := entry["integrity"]; ok {
		t.Errorf("stale integrity must be cleared: %+v", entry)
	}
	data, _ := res.Data.(map[string]any)
	if note, _ := data["note"].(string); !strings.Contains(note, "global_name") {
		t.Errorf("the note must ask for the new global_name: %q", note)
	}

	// Supplying the new global in the same call keeps it.
	res, err = (&LibraryAdd{Library: map[string]any{"name": "qr", "url": "https://cdn.example/newer-qr.js", "global_name": "qrcode"}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("run: %v %+v", err, res)
	}
	if got := libraryEntry(t, db, "qr")["global_name"]; got != "qrcode" {
		t.Errorf("global_name = %v, want qrcode", got)
	}

	// Changing something other than the url leaves the file's fields alone.
	res, err = (&LibraryAdd{Library: map[string]any{"name": "qr", "url": "https://cdn.example/newer-qr.js", "purpose": "share QR"}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("run: %v %+v", err, res)
	}
	if got := libraryEntry(t, db, "qr")["global_name"]; got != "qrcode" {
		t.Errorf("a non-url update must keep global_name, got %v", got)
	}
}

// A kind=module library without module_name is dropped from the importmap in silence,
// so it is refused where the fix is one field away.
func TestAddLibrary_ModuleNeedsModuleName(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[]}`)
	ctx := &tools.ToolContext{DB: db}
	res, err := (&LibraryAdd{Library: map[string]any{
		"name": "three.js", "kind": "module",
		"url": "https://cdn.jsdelivr.net/npm/three@0.185.1/build/three.module.js",
	}}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "module_name") {
		t.Fatalf("a module library without module_name must be refused, got %+v", res)
	}
	if names := libraryNames(t, db); len(names) != 0 {
		t.Fatalf("refused library must not be added, got %v", names)
	}
}

// A classic script without global_name still loads, so it is added with a warning;
// one that names its global, and a stylesheet, get none.
func TestAddLibrary_WarnsOnClassicScriptWithoutGlobal(t *testing.T) {
	db := setupPlanDB(t, `{"external_libraries":[]}`)
	ctx := &tools.ToolContext{DB: db}
	warning := func(lib map[string]any) any {
		t.Helper()
		res, err := (&LibraryAdd{Library: lib}).Run(ctx)
		if err != nil || !res.Success {
			t.Fatalf("add %v: err=%v res=%+v", lib["name"], err, res)
		}
		data, _ := res.Data.(map[string]any)
		return data["warning"]
	}
	if w := warning(map[string]any{"name": "marked", "url": "https://cdn.example/marked.min.js"}); w == nil {
		t.Error("a classic script without global_name must carry a warning")
	}
	if w := warning(map[string]any{"name": "Chart.js", "url": "https://cdn.example/chart.umd.js", "global_name": "Chart"}); w != nil {
		t.Errorf("a classic script with global_name must not warn, got %v", w)
	}
	if w := warning(map[string]any{"name": "Inter", "url": "https://fonts.googleapis.com/css2?family=Inter&display=swap"}); w != nil {
		t.Errorf("a stylesheet defines no global and must not warn, got %v", w)
	}
}
