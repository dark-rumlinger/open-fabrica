package probe

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/network"
)

func TestLoadFailureKind(t *testing.T) {
	cases := []struct {
		name       string
		rt         network.ResourceType
		sameOrigin bool
		want       string
	}{
		{"same-origin script is hard", network.ResourceTypeScript, true, "load_failed"},
		{"same-origin stylesheet is hard", network.ResourceTypeStylesheet, true, "load_failed"},
		{"same-origin document is hard", network.ResourceTypeDocument, true, "load_failed"},
		{"same-origin xhr is hard", network.ResourceTypeXHR, true, "load_failed"},
		{"same-origin fetch is hard", network.ResourceTypeFetch, true, "load_failed"},
		{"same-origin image is soft", network.ResourceTypeImage, true, "load_failed_asset"},
		{"same-origin font is soft", network.ResourceTypeFont, true, "load_failed_asset"},
		{"same-origin media is soft", network.ResourceTypeMedia, true, "load_failed_asset"},
		{"same-origin other is soft", network.ResourceTypeOther, true, "load_failed_asset"},
		// A cross-origin CDN script (e.g. Tailwind) failing in the probe must
		// be a warning, not a fatal crash.
		{"cross-origin script is soft", network.ResourceTypeScript, false, "load_failed_asset"},
		{"cross-origin stylesheet is soft", network.ResourceTypeStylesheet, false, "load_failed_asset"},
		{"cross-origin document is soft", network.ResourceTypeDocument, false, "load_failed_asset"},
		{"cross-origin image is soft", network.ResourceTypeImage, false, "load_failed_asset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := loadFailureKind(c.rt, c.sameOrigin); got != c.want {
				t.Errorf("loadFailureKind(%v, sameOrigin=%v) = %q, want %q", c.rt, c.sameOrigin, got, c.want)
			}
		})
	}
}

func TestHostOf(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:5733/", "127.0.0.1"},
		{"http://app.localhost:5733/files/js/main.js", "app.localhost"},
		{"https://CDN.TailwindCSS.com/", "cdn.tailwindcss.com"},
		{"https://fonts.googleapis.com/css2?family=Space+Grotesk", "fonts.googleapis.com"},
		{"data:text/javascript,console.log(1)", ""},
		{"about:blank", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := hostOf(c.in); got != c.want {
			t.Errorf("hostOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadFailureKind_SameProjectHostStaysHard documents the end-to-end intent: the
// project's own host stays hard, the project's own served files stay hard, but
// a declared third-party CDN on a different host is downgraded.
func TestLoadFailureKind_SameProjectHostStaysHard(t *testing.T) {
	probeHost := hostOf("http://app.localhost:5733/")
	// A same-origin /files/ script.
	if probeHost != hostOf("http://app.localhost:5733/files/js/main.js") {
		t.Fatal("same project host should match its own /files/ asset")
	}
	// Tailwind is a different host.
	if probeHost == hostOf("https://cdn.tailwindcss.com/") {
		t.Fatal("project host must not match the Tailwind CDN host")
	}
}

// TestRuntimeSignature pins the runtime-signature discriminator: two throwing
// layers of one bug (revealed one after another as each is fixed) get distinct
// signatures, while the SAME layer re-observed with drifted line:col, host, and
// ?v= cache-buster gets an identical signature. This is what lets the fix-loop
// re-seed a newly-revealed site without looping on a persistent one.
func TestRuntimeSignature(t *testing.T) {
	frame := func(fn, file, lc string) string {
		return "\n    at " + fn + " (http://azure-falcon-j6byl.localhost:5733/files/" + file + ":" + lc + ")"
	}
	entryFrame := func(v, lc string) string {
		return "\n    at init (http://azure-falcon-j6byl.localhost:5733/files/game.entry.js?v=" + v + ":" + lc + ")"
	}
	const hdr = `[console.error] "hook onPlanetArrival failed" TypeError: rng is not a function`

	layer1 := []string{
		hdr + frame("makeFloraMesh", "game.terrain.js", "218:30") + frame("buildFlora", "game.terrain.js", "178:18") + entryFrame("3184fbe2", "66:5"),
		hdr + frame("makeCreature", "game.creatures.js", "94:40") + frame("spawnCreatures", "game.creatures.js", "68:22"),
	}
	layer2 := []string{
		hdr + frame("genFloraName", "game.terrain.js", "280:25") + frame("buildFlora", "game.terrain.js", "185:30") + entryFrame("3184fbe2", "66:5"),
		hdr + frame("genCreatureName", "game.creatures.js", "149:25") + frame("spawnCreatures", "game.creatures.js", "72:38"),
	}

	s1, s2 := runtimeSignature(layer1), runtimeSignature(layer2)
	if s1 == "" || s2 == "" {
		t.Fatalf("a project throw must yield a non-empty signature: %q / %q", s1, s2)
	}
	if s1 == s2 {
		t.Fatalf("distinct throwing layers must have distinct signatures, both = %q", s1)
	}
	if !strings.Contains(s1, "makeFloraMesh@game.terrain.js") || !strings.Contains(s2, "genFloraName@game.terrain.js") {
		t.Fatalf("signature must key on the throwing frames:\n s1=%q\n s2=%q", s1, s2)
	}

	// Same defect, drifted line:col + host + ?v= must reduce to the same signature.
	layer1Rerun := []string{
		hdr + frame("makeFloraMesh", "game.terrain.js", "219:31") + "\n    at buildFlora (http://other.localhost:9111/files/game.terrain.js:179:18)" + entryFrame("ff00ff00", "66:5"),
		hdr + frame("makeCreature", "game.creatures.js", "95:41") + frame("spawnCreatures", "game.creatures.js", "69:22"),
	}
	if got := runtimeSignature(layer1Rerun); got != s1 {
		t.Fatalf("signature must be stable across line/host/query drift:\n first: %q\n rerun: %q", s1, got)
	}

	// No project frame -> empty signature, so the coarse identity stays in force.
	if got := runtimeSignature([]string{"[load_failed] 404 https://cdn.example.com/three.js (net::ERR_ABORTED)"}); got != "" {
		t.Fatalf("a hard error with no project frame must yield an empty signature, got %q", got)
	}
}
