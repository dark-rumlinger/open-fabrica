package analysis

import (
	"reflect"
	"testing"
)

func TestProjectModuleName(t *testing.T) {
	cases := map[string]string{
		"js/world.js":         "world",
		"js/world-gen.js":     "world-gen",
		"js/island-render.js": "island-render",
		"main.mjs":            "main",
		"a/b/c/state.js":      "state",
		"noext":               "noext",
		"":                    "",
		"/":                   "",
	}
	for in, want := range cases {
		if got := ProjectModuleName(in); got != want {
			t.Errorf("ProjectModuleName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every module answers to both its logical name and its full basename, so an
// import resolves whether it names the module or the file.
func TestProjectModuleMap_UniqueBasenames(t *testing.T) {
	paths := []string{"js/state.js", "js/world-gen.js", "js/main.js"}
	byName, collisions := ProjectModuleMap(paths)
	if len(collisions) != 0 {
		t.Fatalf("unexpected collisions: %v", collisions)
	}
	want := map[string]string{
		"state":        "js/state.js",
		"state.js":     "js/state.js",
		"world-gen":    "js/world-gen.js",
		"world-gen.js": "js/world-gen.js",
		"main":         "js/main.js",
		"main.js":      "js/main.js",
	}
	if !reflect.DeepEqual(byName, want) {
		t.Errorf("byName = %v, want %v", byName, want)
	}
}

// The dotted-filename trap: "x.state.js" strips only its last extension, so the
// logical name is "x.state", yet a model that knows the filename imports
// "x.state.js". Both forms must resolve, to the same file.
func TestProjectModuleMap_DottedFilenameResolvesBothForms(t *testing.T) {
	byName, collisions := ProjectModuleMap([]string{"club.state.js", "club.avatars.js"})
	if len(collisions) != 0 {
		t.Fatalf("unexpected collisions: %v", collisions)
	}
	for _, spec := range []string{"club.state", "club.state.js"} {
		if got, ok := ResolveProjectModuleSpecifier(spec, byName); !ok || got != "club.state.js" {
			t.Errorf("specifier %q resolved to %q ok=%v, want club.state.js", spec, got, ok)
		}
	}
}

func TestProjectModuleMap_CollisionIsDroppedNotGuessed(t *testing.T) {
	// Two modules share basename "state": ambiguous, so neither is exposed and the
	// clash is reported; the platform never silently picks one. Aliasing must not
	// become a back door that resolves a name the logical form correctly refused.
	paths := []string{"a/state.js", "b/state.js", "js/main.js"}
	byName, collisions := ProjectModuleMap(paths)
	for _, spec := range []string{"state", "state.js"} {
		if _, present := byName[spec]; present {
			t.Errorf("colliding name %q must not be in byName, got %v", spec, byName)
		}
	}
	if byName["main"] != "js/main.js" {
		t.Errorf("non-colliding 'main' should still map, got %v", byName)
	}
	if !reflect.DeepEqual(collisions, []string{"state", "state.js"}) {
		t.Errorf("collisions = %v, want [state state.js]", collisions)
	}
}

func TestResolveProjectModuleSpecifier(t *testing.T) {
	byName := map[string]string{"world": "js/world.js", "state": "js/state.js"}

	if p, ok := ResolveProjectModuleSpecifier("world", byName); !ok || p != "js/world.js" {
		t.Errorf(`bare "world" should resolve to js/world.js, got %q ok=%v`, p, ok)
	}
	// "three" is an external lib (importmap CDN entry), not an internal module.
	if _, ok := ResolveProjectModuleSpecifier("three", byName); ok {
		t.Errorf(`external "three" must not resolve as an internal module`)
	}
	// Relative and absolute specifiers are ResolveImport's job, not this one.
	if _, ok := ResolveProjectModuleSpecifier("./world.js", byName); ok {
		t.Errorf("relative specifier must return ok=false here")
	}
	if _, ok := ResolveProjectModuleSpecifier("/files/js/world.js", byName); ok {
		t.Errorf("absolute specifier must return ok=false here")
	}
}

// A reserved platform specifier (ofnet/e2ee/auth) resolves to its platform path even
// when a stray user module shares the basename - so the helper import can never be
// silently knocked out of the importmap by a colliding user file (defense in depth;
// the save gate normally rejects such a user save before it lands).
func TestProjectModuleMap_ReservedNameWinsOnCollision(t *testing.T) {
	paths := []string{"lib/ofnet.js", "js/ofnet.js", "js/main.js"}
	byName, collisions := ProjectModuleMap(paths)
	// Both forms of the reserved specifier hold the platform path: the helper is
	// imported as 'ofnet', but 'ofnet.js' must not resolve to the stray user file
	// either.
	for _, spec := range []string{"ofnet", "ofnet.js"} {
		if byName[spec] != "js/ofnet.js" {
			t.Errorf("reserved %q must resolve to js/ofnet.js on collision, got %q", spec, byName[spec])
		}
		for _, c := range collisions {
			if c == spec {
				t.Errorf("reserved %q must not be reported as an unresolved collision", spec)
			}
		}
	}
	if byName["main"] != "js/main.js" {
		t.Errorf("non-colliding 'main' should still map, got %v", byName)
	}
}

func TestIsReservedModuleSpecifier(t *testing.T) {
	for _, n := range []string{"ofnet", "e2ee", "auth"} {
		if !IsReservedModuleSpecifier(n) {
			t.Errorf("%q should be a reserved platform module specifier", n)
		}
	}
	if IsReservedModuleSpecifier("world") {
		t.Errorf("'world' is a normal user module name, not reserved")
	}
}

// The known-globals allowlist must cover the modern web platform a realtime/media
// app hand-writes (WebRTC, media capture, web crypto types) so bare use does not
// trip the undefined-reference gate.
func TestIsJSGlobal_ModernWebPlatform(t *testing.T) {
	for _, name := range []string{
		"RTCPeerConnection", "RTCSessionDescription", "RTCIceCandidate", "RTCDataChannel",
		"MediaStream", "MediaStreamTrack", "MediaRecorder", "MediaDevices",
		"CryptoKey", "SubtleCrypto", "BroadcastChannel", "ReadableStream",
	} {
		if !IsJSGlobal(name) {
			t.Errorf("%q must be a known JS/DOM global (WebRTC/media apps use it bare)", name)
		}
	}
}
