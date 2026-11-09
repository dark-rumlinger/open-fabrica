---
id: module-scope
title: ES module scope contract
kind: prose
governs: []
probes: [is not defined]
---
Every project JS file is an ES module with its own isolated scope - nothing you
define is ambient. A name is usable only where it is declared or imported; a
sibling's symbol arrives ONLY via `export` there plus `import` here, by bare
logical name (`/files/world.state.js` -> `import { x } from 'world.state'`). Any
other bare name throws `<name> is not defined` when that line runs (the save
gate rejects the write) - check the owner's exact exported spelling and import
exactly that.

Reaching a sibling's symbol through a `window.<name>` of your own is the quiet
version of the same mistake: `window` is a real object, so a read you never
assign (or misspelled) does not throw - it is `undefined`, and the wiring it
drives is dead while imports, the module graph and the page probe all stay
green. Cross a file boundary only by importing, never through a `window.*`
global you set; the save gate rejects such a bridge too.
