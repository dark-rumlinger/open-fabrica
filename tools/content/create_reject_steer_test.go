package content

import (
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The defect this closes: a new module's FIRST save carries a binding error (a
// `const` reassigned - a `Cannot assign to "ctx"` the plain syntax parse misses but
// the bundler catches). staticErrorResult rejects the
// create, which persists nothing, yet its message said "fix the named line, then retry" -
// steering the model to patch a file that does not exist. The follow-up patch bounces off
// notFoundForModify, wasting an iteration before the model re-emits the whole body. The
// create-reject must instead steer straight to a full-body re-save.
func TestSave_CreateReject_SteersToFullResave(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	// A brand-new module (no prior artifact) whose body reassigns a const - a binding
	// error the single-file syntax parse passes but the bundler proves.
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "voxel.net.js",
		Body: `export function initNet(){
  const ctx = 1;
  ctx = 2;
  return ctx;
}`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a create with a binding error must be rejected, got %+v", res)
	}
	if res.Code != "static_error" {
		t.Errorf("Code = %q, want static_error", res.Code)
	}
	// The reject must reframe recovery as a full-body re-save, not a patch of a file that
	// was never written.
	if !contains(res.Error, "does not exist yet") || !contains(res.Error, "full corrected body") {
		t.Errorf("create-reject must steer to a full-body re-save, got: %s", res.Error)
	}
}

// The counterpart: the SAME binding error introduced by an UPDATE to an already-saved
// file. The prior artifact survives the reject, so a targeted patch/edit stays valid and
// the message must NOT tell the model the file does not exist - the create-steer is a
// no-op on the update path.
func TestSave_UpdateReject_KeepsEditAndRetry(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	// A clean prior version exists.
	seedJS(t, db, "voxel.net.js", `export function initNet(){
  const ctx = 1;
  return ctx;
}`)

	// Re-save it with a const reassignment - now an update, not a create.
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "voxel.net.js",
		Body: `export function initNet(){
  const ctx = 1;
  ctx = 2;
  return ctx;
}`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("an update introducing a binding error must be rejected, got %+v", res)
	}
	if res.Code != "static_error" {
		t.Errorf("Code = %q, want static_error", res.Code)
	}
	if contains(res.Error, "does not exist yet") {
		t.Errorf("an update reject must not claim the file does not exist, got: %s", res.Error)
	}
}

// steerCreateRejectToResave is a no-op on a nil rejection and on an update (exists=true),
// and appends the re-save steer only on a create reject.
func TestSteerCreateRejectToResave(t *testing.T) {
	if got := steerCreateRejectToResave(nil, "file", "x.js", false); got != nil {
		t.Errorf("nil rejection must pass through, got %+v", got)
	}
	update := &tools.Result{Success: false, Code: "static_error", Error: "boom - fix the named line, then retry."}
	if got := steerCreateRejectToResave(update, "file", "x.js", true); contains(got.Error, "does not exist yet") {
		t.Errorf("update (exists=true) must not gain the create-steer, got: %s", got.Error)
	}
	create := &tools.Result{Success: false, Code: "static_error", Error: "boom - fix the named line, then retry."}
	got := steerCreateRejectToResave(create, "file", "voxel.net.js", false)
	if !contains(got.Error, "does not exist yet") || !contains(got.Error, "full corrected body") {
		t.Errorf("create (exists=false) must gain the full-resave steer, got: %s", got.Error)
	}
}
