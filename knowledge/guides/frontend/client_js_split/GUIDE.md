---
slug: client_js_split
role: capability
title: Client JS module split
summary: Splitting a page's client JS into modules: the importmap and asset scope, the size caps, the one-directional import tree (entry as the unimported root), and the cyclic-import / boot-wiring footguns.
category: frontend
position: 32
structural: [client_split]
---

# Client JS module split

When a page spans multiple `kind=file` JS modules, imports must form a **one-directional tree with the entry as the unimported root** and a **zero-import store module** for shared state/handles:

```js
// /files/app.state.js - zero-import STORE module: state, DOM refs, live-binding engine handles
export const store = { tool: 'brush' };
export let scene = null;
export function setEngine(h) { scene = h.scene; }
export const hooks = { onReply: null }; // optional notify-back channel

// /files/app.tools.js - leaf: imports store, NEVER the entry
import { store, scene, hooks } from 'app.state';
export function initTools() { /* safe: runs after entry calls setEngine() */ }

// /files/app.js - entry: boot only; exports nothing, nothing imports it
import { setEngine } from 'app.state';
import { initTools } from 'app.tools';
setEngine({ scene: makeScene() });
initTools();
```

- **Default to one `kind=file` per page** unless the plan splits subsystems or file size approaches the caps (~24 KB advisory, ~36 KB `module_too_large_client` refusal, ~48 KB `oversized_body` hard stop). Never collapse planned leaves back into the spine.
- **Keep the entry boot-only:** it exports nothing and only `attach_to`s the page; imported leaves resolve transitively (`scope` only picks which pages carry a file's tag, not load order). Move any symbol shared by two leaves into the zero-import store module.
- **External libraries:** import `kind:"module"` libs (`import * as THREE from 'three';`) directly in every file that uses them. Read classic `window.<lib>` globals in one store module (`export const md = window.marked;`) and import from there.
- **Notify-back without cycles:** if leaf B must notify leaf A without importing it, declare `hooks` or `subscribe` in the zero-import store module and call required hooks unguarded (`hooks.onReply(...)`, never `?.`) so missing wiring throws instead of failing silently.
- **Never call through a live binding at leaf top-level:** `export let net = null` is still `null` while leaves evaluate (before the entry calls `setNet(myNet)`). Put `net.on(...)` / `scene.add(...)` inside an exported `init*()` function called by the entry after setting the binding.

<!-- extended -->

## Multi-leaf splits and DOM anchors
> when: a split has several domain leaves or caches DOM refs.

- Check `project_state` **Module contracts** for which leaf exports a symbol before importing it; never guess or re-route through `window.*` bridges or dynamic `import()` workarounds (only `window.__routeParams` and `window.__ofProbeViolations` are exempt).
- Cache DOM refs only for IDs declared in `project_state` **Page DOM anchors** (add missing elements to the page markup first).
- Never swallow a required subsystem/boot error with `console.warn`-and-continue; let required boot failures throw so probes catch them.
- Never overload a single `ctx.*` key for two different objects (e.g. module handle vs 3D object).
