package content

import (
	"testing"

	"openfabrica/tools"
)

// A user module whose basename derives a reserved platform specifier (ofnet / e2ee
// / auth) must be rejected: it would collide with the platform helper in the
// importmap and silently knock BOTH out of ProjectModuleMap. The reserved name is
// the platform's; the model imports the helper, it never authors a same-named file.
func TestSave_ReservedModuleName_Rejected(t *testing.T) {
	for _, name := range []string{"ofnet", "e2ee", "auth"} {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
		res, err := (&ArtSave{
			Kind:     "file",
			FilePath: "lib/" + name + ".js",
			Body:     `export function boot(){ return 1; }`,
		}).Run(ctx)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if res == nil || res.Success {
			t.Fatalf("saving a user module that resolves to reserved specifier %q must be rejected, got %+v", name, res)
		}
		if res.Code != "reserved_module_name" {
			t.Errorf("[%s] Code = %q, want reserved_module_name", name, res.Code)
		}
	}
}

// A regression this pins: a client module that imports a platform
// helper BY SPECIFIER (not window.OFE2EE) and uses a modern browser global
// (RTCPeerConnection / MediaStream) must save cleanly - the helper resolves through
// the importmap, and the WebRTC/media globals are in the known-globals allowlist, so
// neither window_global_bridge nor undefined_reference fires.
func TestSave_ImportsHelperAndUsesWebRTC_Clean(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	// The real e2ee helper is platform-emitted; a stub exporting `session` is enough
	// to exercise specifier resolution here.
	seedJS(t, db, "js/e2ee.js", `export function session(){ return { encrypt(){}, decrypt(){} }; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "call.js",
		Body: `import { session } from 'e2ee';
export function startCall(){
  const pc = new RTCPeerConnection();
  const s = session({ roomId: 'r', password: 'p' });
  const stream = new MediaStream();
  return { pc, s, stream };
}`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("importing a helper by specifier + using RTCPeerConnection/MediaStream must save cleanly, got %+v", res)
	}
}

// Resolution really checks the helper's export set: importing a name the module does
// NOT export is a genuine dangling import and must still be rejected.
func TestSave_ImportsMissingHelperExport_Rejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "js/e2ee.js", `export function session(){ return {}; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "call.js",
		Body: `import { nope } from 'e2ee';
export function go(){ return nope(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("importing a name the helper does not export must be rejected, got %+v", res)
	}
}
