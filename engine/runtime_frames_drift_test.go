package engine

// Runtime<->docs drift guard.
//
// The other consistency tests compare docs<->contracts and docs<->tool
// registry, but nothing ties the realtime docs to the actual runtime that emits
// and consumes the frames (server/public, internal/roomactor). This closes that
// gap for the two enumerable runtime surfaces: the reducer `ctx` API and the
// actor's `@lifecycle` events are pinned to a canonical spec (a rename in the
// runtime fails here, forcing a conscious update of the spec AND the guide) and
// asserted documented; the client-facing WS frame keys must still be emitted by
// the runtime and stay documented. String-level on purpose: they read the
// sibling source as text via runtime.Caller so they cannot be fooled by a stale
// build, and they fail loudly rather than vacuously (a broken path yields an
// empty set the set-equality assertion reports).

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"openfabrica/knowledge"
	"openfabrica/pack"
)

// moduleRoot resolves the repo root from this test file's own location, so the
// guard reads sibling-package source deterministically regardless of the test's
// working directory (mirrors toolArgFields' runtime.Caller approach).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	return filepath.Dir(filepath.Dir(thisFile)) // engine/ -> repo root
}

func readSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// resolveContracts expands {{contract:id}} the way the assembler does, so a
// documentation check reads what the model is actually served rather than the
// unrendered source. It calls the assembler's own transclusion rather than
// re-implementing it: a private pattern here would quietly resolve a different set
// of tokens than the prompt does, and this guard's whole job is to read the prompt.
func resolveContracts(doc string) string {
	return pack.TranscludeContracts(doc)
}

// knowledgeCorpus concatenates every embedded markdown doc (guides, contracts,
// mental-model, stages) - the "is this documented anywhere the model reads"
// corpus. Uses the embedded FS so it matches exactly what ships in the binary.
func knowledgeCorpus(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	err := fs.WalkDir(knowledge.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, rerr := knowledge.FS.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sb.Write(b)
		sb.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("walking knowledge.FS: %v", err)
	}
	if sb.Len() < 1000 {
		t.Fatalf("knowledge corpus is implausibly small (%d bytes) - did the embed break?", sb.Len())
	}
	return sb.String()
}

// --- canonical runtime spec (update deliberately, alongside the docs) ---------

// canonicalReducerCtx is the exact set of methods the reducer sandbox injects on
// `ctx` (internal/roomactor/reducer.go). canonicalReducerEvents is the exact set
// of synthesized lifecycle events the room actor delivers (server/public/
// room_actor.go). Both are the authoritative-games guide's contract with reducer
// authors; changing the runtime set without updating this spec AND the guide is
// the drift this file exists to catch.
var (
	canonicalReducerCtx = []string{
		"random", "now", "players", "reject",
		"after", "cancel", "insert", "update", "upsert",
	}
	canonicalReducerEvents = []string{"@init", "@join", "@leave", "@timer", "@tick"}
)

// reducerDocTokens maps each ctx method to the doc form(s) that prove the guide
// documents it. Names that are ordinary English words (now/players/insert/update)
// require the `ctx.`-qualified form so a stray prose mention can't satisfy them.
var reducerDocTokens = map[string][]string{
	"random":  {"random("},
	"now":     {"now("},
	"players": {"ctx.players"},
	"reject":  {"reject("},
	"after":   {"ctx.after"},
	"cancel":  {"ctx.cancel"},
	"insert":  {"ctx.insert"},
	"update":  {"ctx.update"},
	"upsert":  {"ctx.upsert"},
}

// runtimeFrameKeys are the client-facing WS frame fields the docs promise and the
// runtime emits. Each MUST still be emitted (a rename silently breaks every
// client) AND stay documented.
var runtimeFrameKeys = []string{
	"_type", "_clientId", "_clients", "_room",
	"_peers", "_sender", "_role", "_meta", "_capacity", "_joinToken",
	"_to", "states", "room_field",
}

var (
	reCtxSet     = regexp.MustCompile(`ctx\.Set\("([a-zA-Z]+)"`)
	reActorEvent = regexp.MustCompile(`"(@[a-zA-Z]+)"`)
)

// TestReducerRuntimeMatchesCanonicalSurface pins the reducer ctx API and the
// actor's lifecycle events to the canonical spec above. A method or event added,
// removed, or renamed in the runtime fails here; nothing else catches it until a
// build breaks mid-run.
func TestReducerRuntimeMatchesCanonicalSurface(t *testing.T) {
	ctxGot := uniqueSubmatches(reCtxSet, readSource(t, "internal/roomactor/reducer.go"))
	assertSetEqual(t, "reducer ctx.Set() methods (internal/roomactor/reducer.go)",
		canonicalReducerCtx, ctxGot,
		"update canonicalReducerCtx AND the authoritative-games section of knowledge/guides/realtime/realtime_signaling/GUIDE.md")

	evGot := uniqueSubmatches(reActorEvent, readSource(t, "server/public/room_actor.go"))
	assertSetEqual(t, "room actor @lifecycle events (server/public/room_actor.go)",
		canonicalReducerEvents, evGot,
		"update canonicalReducerEvents AND the guide to match the runtime")
}

// TestReducerSurfaceDocumented asserts every runtime ctx method and lifecycle
// event is actually documented in the realtime guide, so the model is told the
// primitive exists. Transclusions are resolved first: a fact the guide pulls from
// a contract IS documented, and requiring the guide to also spell it out inline
// would demand the restatement the single-home rule exists to prevent.
func TestReducerSurfaceDocumented(t *testing.T) {
	guide := resolveContracts(readSource(t, "knowledge/guides/realtime/realtime_signaling/GUIDE.md"))
	for _, m := range canonicalReducerCtx {
		toks, ok := reducerDocTokens[m]
		if !ok {
			t.Errorf("no doc token registered for ctx.%s - add one to reducerDocTokens", m)
			continue
		}
		if !containsAny(guide, toks) {
			t.Errorf("ctx.%s is injected by the reducer runtime but the authoritative-games guide documents none of %v", m, toks)
		}
	}
	for _, ev := range canonicalReducerEvents {
		if !strings.Contains(guide, ev) {
			t.Errorf("lifecycle event %s is delivered by the room actor but is not documented in the realtime guide", ev)
		}
	}
}

// TestRuntimeFrameKeysDocumentedAndEmitted checks each client-facing frame key
// both ways: still emitted by the runtime (catches a rename that would break
// clients) and still documented somewhere in the knowledge tree.
func TestRuntimeFrameKeysDocumentedAndEmitted(t *testing.T) {
	runtimeUnion := readSource(t, "server/public/ws_hub.go") +
		readSource(t, "server/public/room_actor.go") +
		readSource(t, "server/public/stream.go") +
		readSource(t, "server/public/stream_ws.go")
	docs := knowledgeCorpus(t)
	for _, k := range runtimeFrameKeys {
		if !strings.Contains(runtimeUnion, `"`+k+`"`) {
			t.Errorf("frame key %q is documented but no longer emitted by the runtime (ws_hub/room_actor/stream) - a rename would silently break every client; reconcile docs and runtime", k)
		}
		if !strings.Contains(docs, k) {
			t.Errorf("frame key %q is emitted by the runtime but is not documented anywhere in the knowledge tree", k)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func uniqueSubmatches(re *regexp.Regexp, src string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func assertSetEqual(t *testing.T, label string, want, got []string, fixHint string) {
	t.Helper()
	wset := map[string]bool{}
	for _, x := range want {
		wset[x] = true
	}
	gset := map[string]bool{}
	for _, x := range got {
		gset[x] = true
	}
	var missing, extra []string
	for _, x := range want {
		if !gset[x] {
			missing = append(missing, x)
		}
	}
	for _, x := range got {
		if !wset[x] {
			extra = append(extra, x)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	t.Errorf("%s drifted from the canonical spec.\n  runtime-only (add to the spec + docs): %v\n  spec-only (runtime dropped/renamed it): %v\n  fix: %s",
		label, extra, missing, fixHint)
}

func containsAny(s string, toks []string) bool {
	for _, tok := range toks {
		if strings.Contains(s, tok) {
			return true
		}
	}
	return false
}
