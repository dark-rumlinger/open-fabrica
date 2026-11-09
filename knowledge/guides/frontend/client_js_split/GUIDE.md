---
slug: client_js_split
role: capability
title: Client JS module split
summary: Splitting a page's client JS into modules: the importmap and asset scope, the size caps, the one-directional import tree (entry as the unimported root), and the cyclic-import / boot-wiring footguns.
category: frontend
position: 32
structural: [client_split]
---

# Client JS module split (any page split into multiple JS files)

When a page's behaviour spans more than one `kind=file` JS module - a complex
*application* page (editor, game, spreadsheet, canvas world), a page pulling an
engine library, or any subsystem split the plan carved - the import graph itself
becomes load-bearing. Get the shape wrong and the page boots with its wiring dead
while every static gate and probe stays green. These are the rules that keep a
split sound.

**Every project JS file is an importmap ES module**, imported by its bare logical name
(the module-scope law), never a relative path or a hand-written `<script>` tag (a
`./` specifier is normalized at write). Modules are
deferred, so the DOM is ready and there is **no load-order lever** - the import
graph alone decides what runs. `scope` (`page` default / `global`) only picks
which pages carry a file's tag, not what it imports: a global module may import a
page-scoped leaf and back. The platform helpers ride the same importmap -
`import { apiFetch } from 'auth'`, `from 'ofnet'`, `from 'e2ee'`: by specifier,
never a `window.*` global, never re-implemented. A bare `apiFetch()` you did not
import throws (save auto-inserts it when exactly one module exports the name,
`auto_imported`; otherwise it is rejected as `undefined_reference`). A declared external module library rides it the same way: EVERY
file that touches it carries its own import (`import * as THREE from 'three';`) -
never funnelled through the entry, the store module, or a ctx/hooks bag. Your OWN shared symbols follow the same rule - a zero-import store module plus a static
`import`, never a cross-file `window.*` bridge (rejected at write: `window_global_bridge`
when two or more files touch the same `window.<name>`; `window_dead_global` when a
`window.__<name>` is read and nothing assigns it);
a dynamic `import()` is for genuine lazy-loading, not a way to dodge a static import. The only exempt `window.__*` reads are the platform's runtime
injections (`window.__routeParams`, `window.__ofProbeViolations`).

Size caps for a single client-JS `save`: an advisory split nudge at ~24KB;
refused at ~36KB as `module_too_large_client`;
hard-stopped at ~48KB as `oversized_body`.

When you split, imports MUST form a one-directional tree with the **entry as the
unimported root**. Concretely:

```js
// /files/app.state.js - zero-import STORE module: store, dom refs, engine handles, formatters, rng
export const store = { tool: 'brush' };
export let scene = null;                    // live binding, filled at boot
export function setEngine(h){ scene = h.scene; }

// /files/app.tools.js - a subsystem leaf: imports the store module, NEVER the entry
import { store, scene } from 'app.state';
export function activate(name){ store.tool = name; }

// /files/app.js - the entry: boot only; nothing imports it, it exports nothing
import { setEngine } from 'app.state';
import { activate } from 'app.tools';
setEngine({ scene: makeScene() });          // fill live bindings, then init
activate('brush');
```
A leaf importing `{ x }` from the entry makes the graph cyclic -> rejected at write
(`cyclic_import`), and so do two leaves importing each other. The rest of this core
says how to keep the tree sound.

## When to split, and into how many files

- **Default to ONE linked `kind=file` per page** - no load-order or export-mismatch
  bugs. Decide seams up front and split a large subsystem into complete files BEFORE
  hitting the `module_too_large_client` cap (~36KB); only if one cohesive file still
  won't fit, save a compact complete first body and grow it with `patch`
  (the one case a file grows across calls).
- **One dedicated boot-only entry - a feature module is never the entry.** When a page
  splits into feature modules - the views of an app (landing, prejoin, call), the subsystems
  of a game or editor (movement, terrain, net, HUD) - the entry is a SEPARATE `<app>.js` that
  imports them, runs the boot sequence (and any `showView`), and exports nothing. Every
  feature module is a leaf, including the FIRST view shown or the main subsystem - do not let
  it double as the entry. A symbol two features share lives in the zero-import store module
  from the start, never in the feature that defined it - otherwise the features import each
  other and the graph cycles on whichever file you save last.
- **The split is NOT only size-triggered.** A subsystem boundary the plan carved (a spine
  task plus leaf tasks, each owning its module at the exact `Owns` path) IS a split boundary
  regardless of byte size. Build each leaf against the spine's exported contract, `complete`
  its task once its behavioural `done_when` holds, and NEVER collapse planned leaves back into
  the spine to save turns - one monolithic body for a
  whole editor/game/spreadsheet ships shallow or broken. Save each module at its `Owns`
  path exactly (`/files/app.state.js`); `js/app.state.js` would be a different file.

## The import tree: entry is the unimported root

- **The entry imports the leaves; a leaf NEVER imports back from the entry** - and
  this holds for engine services, not just data. (Build order and leaf batching are
  the build frame's spine-wiring rule; `attach_to` only the entry - imports resolve
  transitively.)
- **Every symbol a leaf calls on the entry lives in a zero-import store module, not the entry** - the
  renderer/scene/camera/lights, `setMode`, `showLoading`/`finishLoading`, input/pointer-lock,
  the UI glue (tool/mode setters like `setTool`, status-bar / readout / HUD updaters, cursor
  updaters, tooltip / popover / colour-picker openers, shared toolbar / palette / swatch
  builders), AND the state store, cached DOM refs, formatters, a toast. A zero-import store
  module imports no other project module (e.g. `app.state.js` / `app.core.js`); leaves import
  from it, never from the entry. The entry is ONLY the boot sequence: create the renderer/scene,
  import the leaves, run init.
- **An engine handle the entry creates but leaves read** (renderer, scene, camera, lights,
  sun) is a **live-binding export** on the zero-import store module - NOT a `getHooks()`/registry
  lookup and NOT a `window.*` global. The leaf declares `export let scene = null` (+ camera,
  sun…) and a one-line `setEngine({ scene, camera, sun })` setter; the entry calls
  `setEngine(...)` once at boot; each leaf does `import { scene, sun }` and reads the name
  directly (a live binding is current after boot). A `register(name, fn)` registry is ONLY for
  callbacks (the entry's loop collecting `register('update', fn)`, a leaf notifying another -
  next rule), never for reading a scene/camera the entry already made. (Why a registry the
  ENTRY builds silently boots dead is in the extended section.)
- **Two leaves that each need the other: one imports, the other is NOTIFIED - never a mutual
  import.** The recurring cycle is not leaf -> entry but leaf <-> leaf: chat must refresh the
  verdict after a reply while the verdict calls back into chat; the detail view opens the
  editor and the editor refreshes the detail after a save; net reports "joined" to the join
  screen it serves. Keep the call that DOES the work as the import (verdict -> chat). The
  report back never imports: the notifier updates the store and the listener re-renders from
  the store's `subscribe`/`onState`, or the store declares `export const hooks = { onReply:
  null };`, the listener sets `hooks.onReply = fn` in its init, and the notifier calls
  `hooks.onReply(...)` unguarded (no `if (hooks.onReply)` or `?.`: an unassigned hook must
  throw, so the probe and flows catch it). A dynamic `import()` or a `window` global is the
  same loop and is rejected too. The plan's `deps` already say which way the import runs: a
  leaf imports only what it deps on.
- **The entry FILE is the unimported root: it `export`s NOTHING, and no leaf imports from it.**
  A spine TASK's declared `exports` (the checklist's `Exposes` line) follow the same rule:
  author them in the store module, never the entry file. The test is mechanical: a symbol
  DEFINED in the entry but CALLED from a leaf is misplaced -
  it belongs in the zero-import store module. Author the whole shared surface there up front (the
  `entry_exports` / `cyclic_import` gates reject the drift and their messages carry the exact
  relocation, but authoring it right the first time costs no turns).

## Don't call through a live binding before boot sets it

- **A leaf's top-level code must not run against a live binding the entry sets at boot -
  during module evaluation it is still the `null` seed.** Every leaf body executes BEFORE the
  entry's boot line calls the setter (`setEngine`/`setNet`), so a live-binding handle
  (`export let net = null`) is `null` while the leaf is evaluating. A subscription or wiring
  call at the leaf's module scope - `net.on('snapshot', …)`, `scene.add(mesh)`, an
  `initHandlers()` invoked at the bottom of the file - runs against that dead seed: it throws a
  null deref, or silently binds nothing, and the page boots with that subsystem wired to
  nowhere while every gate and probe stays green (the loading screen never advances). Reading a
  live binding is only safe from code that runs AFTER boot: put the setup in an
  `export function initNetHandlers()` and have the entry call it right after `setNet(myNet)`,
  or read the handle inside the rAF loop / an event handler. The test is mechanical: a leaf's
  module-body top level may DECLARE and import, but must not CALL anything through a binding the
  entry has not set yet.

<!-- extended -->

## A multi-leaf split: a symbol lives in exactly ONE leaf - look it up, don't guess
> when: the split has SEVERAL domain leaves (not just entry + one store module), or you are about to import a shared singleton (`net`, `engine`, `state`, `scene`) from a sibling.

- **A large subsystem often splits into SEVERAL domain leaves** (`world.state.js`,
  `world.avatars.js`, `world.net.js`) that import from one another one-directionally
  (a mutual A<->B pair is still a cycle), not one catch-all store module. Each named
  symbol is `export`ed by exactly ONE of them: before you
  `import { applyRemoteAvatar }`, find which leaf EXPORTS it in the `project_state`
  **Module contracts** - do NOT assume the store module owns everything. (A wrong-source
  import of a name exactly one other leaf exports is auto-corrected at write; a name two
  leaves export or none does is a real defect the reject hands back to you.)
- **Referencing a shared singleton (`net`, `engine`, `state`, `scene`) from a leaf is
  two checked steps, every time.** (1) Find the owning leaf's export list in Module
  contracts and import EXACTLY that name: `import { net } from 'world.state'`. (2) If
  the name is NOT in that export list yet, add the live-binding export and its setter
  to the leaf FIRST - in the same turn - then import it. The two failure shapes this
  kills are the most common in a split: using `net` bare (ships `net is not defined`
  from an event handler the probe never runs) and importing a name the leaf never
  exported (rejected at write as `unresolved_import`).

## Bind to the DOM the page DECLARES
> when: writing the leaf that caches a split page's DOM refs (getElementById / querySelector).

- **A split page's leaves cache DOM refs in the zero-import store module, and every one of them
  binds to an id the page markup actually provides.** `project_state`'s **Page DOM
  anchors** section lists those ids per page and says what happens when you reach for one
  that isn't there; read it before you write the ref cache, and add the element to the
  page markup first if the id you want is missing.

## Why a registry the entry builds boots dead
> when: you are tempted to share a scene/camera/manager through a getHooks() bag, a hooks registry, or any object the entry fills in its own body.

Reaching an engine handle back through a persistent `getHooks()` bag or a `window.__*` global
is the indirection that invents a `window` bridge and a stray `getHooks` import - two writes
the gate then rejects. Nor can you dodge the back-import with a registry the entry initializes
in its own body: a module-body `var hooks = {}` / `var hooks = null` runs AFTER the hoisted
leaf imports that populate it and silently wipes every registration, so the page boots with its
wiring dead (the loading screen never advances, the New-Game handler does nothing). The fix is
always the same - push the shared symbol DOWN into the zero-import store module as a live-binding export
(see the core). A `hooks` object DECLARED in the zero-import store module is different and safe:
the store evaluates before every leaf that imports it, so a leaf's `hooks.onX = fn` at init
is never wiped - that is the notify-back channel the core prescribes. The `entry_exports` gate is scoped to a real multi-module spine (an entry that
imports project siblings; a lone one-file page's exports harm nothing) and tolerates a re-save
that carries an unchanged pre-existing surface so a mid-relocation save is not re-trapped -
tolerance is not permission: those exports still have to move.

## Module footguns
> when: wiring an external library, sibling helpers, subsystem registration, or a shared ctx across split modules.

- **A `kind:"module"` external library is NOT shared state - import it directly in EVERY
  file that uses it.** The importmap maps its specifier (`three`, `three/addons/...`), so
  `import * as THREE from 'three';` at the top of each leaf is free and binds `THREE` at
  module-evaluation time. NEVER funnel a library through the entry, the store module, or a
  runtime `ctx`/hooks object (`THREE = ctx.THREE` in init): a module-level
  `const v = new THREE.Vector3()` then runs before `THREE` is assigned and **crashes the
  whole module at load**. Each file that touches the library carries its own import.
- **A THIRD-PARTY library loaded as a classic CDN `<script>` publishes itself on `window`
  (`window.marked`, `window.Chart`) - it has no importmap specifier, so read that global
  from exactly ONE zero-import store module, `export` a binding, and `import` it everywhere else.**
  A second file reading the dot-form `window.marked` trips the window bridge gate (a
  bare global such as `Phaser` or `Chart` used without `window.` passes every gate, so
  the one-store-module rule matters most for the `window.<lib>` form). (Declare that
  global as the library's `global_name` in the plan so the undefined-reference gate
  treats `window.marked` as a known library global, not a typo - without it that gate
  silently goes non-blocking for the whole project.) The fix is one leaf:
  `export const md = window.marked;` there, then `import { md }` in every consumer. Wrapping
  it in a NEW file that also reads `window.marked` while the original readers still do just
  relocates the bridge - strip the `window.<lib>` read from every file in the same turn.
  This launder-in-one-leaf pattern is ONLY for genuine CDN UMD scripts. The PLATFORM helpers
  (auth, realtime, crypto) already HAVE importmap specifiers (see the core's bridge rule):
  import them directly in every leaf that needs them; never launder them through a wrapper
  and never read them off `window`.
- **Importing `{ rng }` from the zero-import store module is the endorsed pattern, never a
  cycle** - the store imports nothing, so no back-edge can exist. Do NOT route around
  an imagined cycle with a lazy getter, a `rng_import()` wrapper, or a re-declared local -
  that leaves the name **unbound** and ships a `ReferenceError`. If you use a name a sibling
  exports, import it at the top of the file, plainly.
- **A bare `foo()` you neither imported nor defined throws the instant it runs.** A
  sibling's function is in your scope ONLY when you `import` it (and the sibling `export`s
  it). Calling a helper that merely EXISTS in
  another leaf - a private, non-exported `function getEventPos(e)` in the paint leaf, say - is
  NOT in scope; it throws the instant that line runs, and because the throw often sits inside
  an event handler (a pointer drag) it never fires under the page-load probe and ships live.
  The save gate rejects this with `undefined_reference` (unless exactly one sibling
  exports the name, in which case the import is added for you). Two fixes: if the helper is
  genuinely shared across leaves (pointer-to-document coordinate conversion, colour math, a
  formatter), put it in the zero-import store module and `export` it, then `import` it wherever needed;
  if it is local, define it in this file. Never assume a sibling's private symbol is in scope.
- **Never swallow a subsystem/boot failure with `console.warn` and continue.** A loader that
  does `for (p of SUBSYSTEMS){ try { (await import(p)).register() } catch (e) { console.warn('load
  failed', p, e) } }` turns a FATAL (a forgotten import, a removed library API) into a page that
  still paints with that subsystem silently dead - the exact way a required feature can ship gone
  while every probe stays green. If a subsystem is required for the page to work, let its error throw (so it
  surfaces as an uncaught error the probe catches) or `console.error` the failing path and stop
  the boot - do not `console.warn`-and-continue. And each subsystem registers by importing
  `register` (or whatever it calls) at the top of its file, plainly - a missing import here is
  invisible until that line runs. Likewise, never **guard a REQUIRED cross-module call** with
  `if (hooks.x) hooks.x()` / `hooks.x?.()`: that swallows a missing wire exactly like the
  `console.warn` loop - the click does nothing, no error throws, every probe stays green. Call
  required wiring directly so an unregistered hook throws and the probe catches it; optional-
  chain only what is genuinely optional.
- **One `ctx` key, one meaning.** Never overload a single `ctx.*` key for two different
  things - e.g. `ctx.ship` as both the Ship MODULE handle and the ship's 3D OBJECT. The
  second assignment silently clobbers the first and every caller of the lost one breaks, then
  you burn turns untangling it. Reserve distinct keys (`ctx.ship` for the object,
  `ctx.shipModule` for the handle) from the first time you wire them.
- **`let` any handle you reassign; never reassign a `const`.** Reusing a name - a second
  `ctx = other.getContext('2d')`, assigning to an imported binding, or re-declaring a name in one
  scope - is a binding error a single-file syntax parse misses, so the write gate catches it and
  names the line. Give each handle its own name, or `let` the one you reassign.
- **`export { ... }` takes names, not declarations.** ES module exports are LIVE bindings, so
  `export { scene }` is enough - callers see later reassignments of a module-scoped `let scene`.
  The pre-save gate rejects the syntax traps here and its rejection carries the fix.
