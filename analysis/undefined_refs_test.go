package analysis

import (
	"fmt"
	"reflect"
	"testing"
)

func refNames(refs []UndefinedRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

// The shape: a private helper defined (not exported) in
// one leaf and called from a sibling that never imports it.
const paintLeaf = `import { screenToDoc } from './editor.canvas.js';

function getEventPos(e) { var pt = screenToDoc(e.clientX, e.clientY); return pt; }
function brushDown(e) { var pos = getEventPos(e); paint(pos); }
function paint(p) { return p; }

export { brushDown };`

const objectsLeaf = `import { state } from './editor.state.js';
import { screenToDoc } from './editor.canvas.js';

function moveMove(e) { var pos = getEventPos(e); return pos; }
function marqueeMove(e) { var p = getEventPos(e); return p; }

export { moveMove, marqueeMove };`

func TestUndefinedCallRefs_ForgottenSiblingHelper(t *testing.T) {
	refs := UndefinedCallRefs("/files/editor.tools-objects.js", objectsLeaf)
	if got := refNames(refs); !reflect.DeepEqual(got, []string{"getEventPos"}) {
		t.Fatalf("objects leaf: got %v, want [getEventPos]", got)
	}
	if refs[0].Line != 4 {
		t.Errorf("first getEventPos line = %d, want 4", refs[0].Line)
	}
}

func TestUndefinedCallRefs_DefiningLeafIsClean(t *testing.T) {
	if refs := UndefinedCallRefs("/files/editor.tools-paint.js", paintLeaf); len(refs) != 0 {
		t.Fatalf("paint leaf (defines getEventPos) should be clean, got %v", refNames(refs))
	}
}

func TestUndefinedCallRefs_NoFalsePositives(t *testing.T) {
	cases := []struct{ name, src string }{
		{"object method shorthand", `var o = { draw(x) { return x + 1; } }; o.draw(1);`},
		{"class methods and new", `class A { constructor() { this.n = 0; } run() { this.step(); } step() {} }
var a = new A(); a.run();`},
		{"property call chains", `ctx.fillRect(0, 0, 1, 1); Math.max(1, 2); obj.a.b.c();`},
		{"arrow and destructured params", `[1].forEach(function(v) { v.toString(); });
const f = ({ a, b }) => a() + b;
const g = ([x]) => x();
const h = z => z();`},
		{"catch binding", `function risky() {} function report(e) { return e; }
try { risky(); } catch (err) { report(err); }`},
		{"default param value", `function noop() {} function k(cb = noop) { cb(); } k();`},
		{"known globals", `setTimeout(function () {}, 0); requestAnimationFrame(tick); function tick() {}
var d = new ImageData(1, 1); var p = new Path2D(); structuredClone({});`},
		{"imports named default namespace", `import def from './x.js';
import * as ns from './y.js';
import { a, b as c } from './z.js';
def(); ns.run(); a(); c();`},
		{"mentions in string and comment", `// ghostFn() must not count
/* neither ghost2() */
var s = "ghost3()"; function real() {} real();`},
		{"implicit global assignment", `handler = function () {}; handler();`},
		{"iife with hoisted inner", `(function () { inner(); function inner() {} })();`},
		{"hoisted use before declaration", `boot(); function boot() {}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if refs := UndefinedCallRefs("/files/m.js", c.src); len(refs) != 0 {
				t.Errorf("expected no undefined refs, got %v", refNames(refs))
			}
		})
	}
}

func TestUndefinedCallRefs_Flags(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"bare undefined call in function", `function a() { ghost(); }`, []string{"ghost"}},
		{"top-level undefined call, deduped", `doThing(); doThing();`, []string{"doThing"}},
		{"one undefined among defined", `function a() { alpha(); beta(); } function beta() {}`, []string{"alpha"}},
		{"member call is not the flag, sibling is", `x.y(); zed();`, []string{"zed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := refNames(UndefinedCallRefs("/files/m.js", c.src)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// refKinds renders each finding as "name:kind@line" for exact assertions.
func refKinds(refs []UndefinedRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s:%s@%d", r.Name, r.Kind, r.Line))
	}
	return out
}

func TestUndefinedRefs_NoFalsePositives(t *testing.T) {
	cases := []struct{ name, src string }{
		// The live-binding leaf itself: it declares the names it assigns.
		{"state leaf assigns its own exports", `export let scene = null;
export function setEngine(h) { scene = h.scene; }`},
		{"class fields", `export const k = 1;
class E { hp = 100; static n = 1; step() { this.hp = 0; } }
new E().step();`},
		{"hex literal is not an identifier", `export const C = 0x05000d;
const D = 0xff00aa;
export function paint(ctx) { ctx.fillStyle = C + D; }`},
		{"destructured and defaulted params with nested parens", `import { getDefault, run } from './x.js';
export function f({ dt = getDefault() }, cb = () => run()) { cb(); return dt.value; }`},
		{"object literal keys and shorthand", `import { cam } from './x.js';
const scene = {};
export const rig = { camera: cam.x, scene };`},
		{"var hoisting out of a block", `export function f(a) { if (a) { var y = 1; } y = 2; return y; }`},
		{"nested closure assigns an outer local", `export function outer() { let v; return function () { v = 1; return v.x; }; }`},
		{"member call chains and known globals", `export const go = () => { ctx().fillRect(0, 0, 1, 1); Math.max(1, 2); window.scrollTo(0, 0); };
function ctx() { return document.createElement('canvas').getContext('2d'); }`},
		{"optional chaining on a declared name", `export function f(opts) { return opts?.a?.b; }`},
		// Browser event constructors: a build lost a fix-pass rewriting a correct
		// `new HashChangeEvent('hashchange')` that this list did not know.
		{"browser event constructors", `export function go(h) {
  window.dispatchEvent(new HashChangeEvent('hashchange'));
  window.dispatchEvent(new PopStateEvent('popstate', { state: h }));
  return [SubmitEvent.name, AnimationEvent.name, TransitionEvent.name, EventTarget.name];
}`},
		{"catch binding used in the handler", `import { report } from './x.js';
export function risky() { try { boom(); } catch (err) { report(err.message); } }
function boom() {}`},
		{"imported live binding is bound", `import { net, setNet } from './world.state.js';
export function send(d) { setNet(d); net.send(d); }`},
		// HTML built with a nested template literal inside a `.map(...)`
		// interpolation: attribute names in the string text must not be read as
		// assignments to undeclared identifiers; every real reference here is bound.
		{"nested template literal html builder", `import { fmt } from './util.js';
export function render(items) {
  return ` + "`<ul>${items.map(it => `<li data-row-id=\"${it.id}\" data-on=\"${it.on}\">${fmt(it.label)}</li>`).join('')}</ul>`" + `;
}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if refs := UndefinedRefs("/files/m.js", c.src); len(refs) != 0 {
				t.Errorf("expected no undefined refs, got %v", refKinds(refs))
			}
		})
	}
}

func TestUndefinedRefs_Flags(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		// A value built in one function and written to a name the module never
		// declared. A parameter of the same name elsewhere must not hide it.
		{"assigned but never declared", `import { setEngine } from './club.state.js';
function buildBooth(scene) { scene.add(1); }
function boot() {
  const sceneLocal = {};
  buildBooth(sceneLocal);
  setEngine({ scene: sceneLocal });
  scene = sceneLocal;
}`, []string{"scene:assign@7"}},
		// A near miss: module scope holds `_camera`, the handler reaches for
		// `camera`. A bare read is not a checked position, so the report lands on
		// the member read rather than on the guard above it.
		{"near-miss against an underscored local", `export const R = 1;
let _camera = null;
function onResize() {
  if (!camera) return;
  camera.aspect = 1;
}
window.addEventListener('resize', onResize);`, []string{"camera:member@5"}},
		// Importing the setter does not bind the value.
		{"imported the setter, not the value", `import { state, setNet } from './world.state.js';
export function sendTickState(dt) {
  if (!state.joined) return;
  net.send({ dt });
}`, []string{"net:member@4"}},
		{"module-scope implicit global assignment", `export const x = 1;
handler = function () {};
handler();`, []string{"handler:assign@2"}},
		{"assignment leads over an earlier read", `export const x = 1;
function draw() { return scene.userData; }
function boot() { scene = {}; }`, []string{"scene:assign@3"}},
		{"forgotten sibling helper in call position", `import { state } from './editor.state.js';
export function moveMove(e) { return getEventPos(e); }`, []string{"getEventPos:call@2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := refKinds(UndefinedRefs("/files/m.js", c.src)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDeclaredTopLevelNames_LocatesPrivateHelper(t *testing.T) {
	bodies := map[string]string{
		"/files/editor.tools-paint.js":   paintLeaf,
		"/files/editor.tools-objects.js": objectsLeaf,
	}
	decl := DeclaredTopLevelNames(bodies)
	if got := decl["getEventPos"]; !reflect.DeepEqual(got, []string{"/files/editor.tools-paint.js"}) {
		t.Errorf("getEventPos declared in %v, want [/files/editor.tools-paint.js]", got)
	}
	// getEventPos is not exported anywhere - so the fix message steers to the
	// declaring module, not to an import.
	if src := ExportIndex(bodies)["getEventPos"]; len(src) != 0 {
		t.Errorf("getEventPos should not be exported, ExportIndex has %v", src)
	}
}
