---
slug: client_view_toggle
role: capability
title: Client view toggles & nav (panels, drawers, tabs, app-shaped views)
summary: The one fact that keeps interactive controls from shipping dead - a panel/drawer/tab/menu reveal must UNDO exactly what hid it, one visibility convention, never a hide-class the handler never removes. Plus how an app-shaped screen's nav switches client-side views. Reach for it on any project with a togglable panel, a nav that swaps views, or a control that shows/hides something.
category: frontend
position: 33
structural: [has_ui]
---

# Client view toggles & nav

The owner's number-one complaint is a button that does nothing. The trap here is
subtler than a handler-less control: the handler RUNS, and the page still looks
dead. A panel is hidden by one class, the open button toggles a DIFFERENT class,
and the two never meet - so the click fires, flips an inert class, and the panel
stays gone. It parses clean, boots green, throws nothing, and passes every write
gate AND the page-load probe (the probe never clicks, and a firing-but-inert
listener still reads as alive). Only the owner, clicking, finds it dead.

**One visibility convention per element. Reveal by undoing exactly what hid it.**
Pick ONE mechanism to control an element's visibility and drive it from both
sides:

- Hidden by a class -> the handler toggles **that same class**:
  `el.classList.toggle('hidden')`, with `.hidden{display:none}` the only rule
  that governs it.
- Revealing with a STATE class instead (`open`, `is-open`, `active`) -> then (a)
  do NOT also leave an always-on hiding class on the element, and (b) ship the
  CSS that makes the state class win:
  `#drawer{display:none} #drawer.open{display:flex}`, or a base off-screen
  transform plus `#drawer.open{transform:none}`.
- Hidden by the boolean `hidden` attribute -> clear the ATTRIBUTE
  (`el.hidden = false` / `el.removeAttribute('hidden')`). A class toggle cannot
  clear `hidden`, so the element stays `display:none` and the control is dead.

A panel that starts `class="... hidden"` while its only reveal is
`classList.toggle('open')` and no `.open` rule shows is flagged after the build as a
dead toggle and sent back for a repair round: the click fires,
`.hidden{display:none}` wins, and the button is dead. Fix it with one of
the two shapes above; never both conventions on one element.

```html
<button id="btnMenu">Menu</button>
<div id="menu" class="panel hidden">...</div>
```
```css
.hidden{ display:none }
```
```js
// RIGHT - toggle the class that actually hides it:
btnMenu.addEventListener('click', () => menu.classList.toggle('hidden'));
// WRONG - fires but nothing shows: `hidden` is never removed, `.open` has no rule:
// btnMenu.addEventListener('click', () => menu.classList.toggle('open'));
```

**Wiring a nav that swaps views in the browser.** When a nav item reveals one
view and hides the others instead of loading a route, wire it on this same
convention: one class hides every view, a click shows the chosen view and hides
its siblings, and the current item is marked active. Each item must resolve to a
view that exists and render it.

```js
const renderers = { home: renderHome, list: renderList }; // one per view
function showView(name){
  document.querySelectorAll('[data-view]').forEach(v =>
    v.classList.toggle('hidden', v.dataset.view !== name));
  document.querySelectorAll('[data-nav]').forEach(n =>
    n.classList.toggle('is-active', n.dataset.nav === name));
  renderers[name]?.(); // render on EVERY show, from current state
}
document.querySelectorAll('[data-nav]').forEach(n =>
  n.addEventListener('click', () => showView(n.dataset.nav)));
showView('home'); // open on a real view, never a blank shell
```

This is the same convention: one class (`hidden`) controls each view's
visibility, and both the toggle and the CSS agree on it.

**A view that is shown must be rendered, not just un-hidden.** When data changes
re-render only the ACTIVE view (the usual store subscription), every hidden view
goes stale, and switching to it reveals old or empty content. Nothing throws and
every view renders fine on its own, so only switching finds it. `showView`
calls the view's renderer every time, for every view. Never special-case a few.

**Two more ways a correct handler still looks dead** - both parse clean and pass the
page-load probe, so recall the section when the case applies:
- **Nothing may sit on top of a control.** A full-viewport layer (touch controls, a
  joystick zone, a vignette, a closed overlay) keeps `pointer-events:none`, restored only
  on its own interactive children - see `nothing-may-sit-on-top-of-a-control`.
- **First paint must be legible.** Never ship above-the-fold content hidden (`opacity:0`,
  an off-screen transform) waiting for JS to reveal it; any entrance animation must leave
  content visible if the module never runs - see `entrance-and-scroll-in-animations`.

<!-- extended -->

## Nothing may sit on top of a control
> when: a full-viewport or absolutely-positioned layer (touch controls, a joystick zone, a vignette, an overlay or modal backdrop) sits over interactive UI.

**A correctly wired control can still be dead: nothing may sit on top of it.**
The second trap is hit-testing. A full-viewport layer (`position:absolute` or
`fixed` with `inset:0` - touch controls, a joystick zone, a vignette, a closed
overlay) painted after the interactive UI eats every click while staying
invisible. Such a layer keeps `pointer-events:none` and restores `auto` only on
its OWN interactive children. Never restore with a blanket
`#parent > * { pointer-events:auto }`: that ID-scoped rule outranks the layer's
own `.overlay{pointer-events:none}` class rule, so the moment the layer is
shown it becomes an invisible click shield over every sibling beneath it. Like
the class mismatch, this parses clean, boots green, and the handler still fires
when called - only a real pointer finds the dead button.

## Entrance and scroll-in animations
> when: content animates in on load or on scroll (an opacity/transform/visibility reveal driven by a JS-added class).

**Content that starts invisible and waits for JS is the same trap at page scale.**
An entrance or scroll-in animation that hides an element by default (`opacity:0`,
an off-screen `transform`, `visibility:hidden`) and only reveals it when JS adds a
class (`.in`, `.is-in`, `.visible`) fails the same silent way: if that JS is
deferred (revealed after an `await fetch`), throws, runs before the node exists,
or the trigger never fires, the element stays invisible. It parses clean, boots
green, and the page-load probe (which never scrolls) sees nothing wrong - only the
visitor sees a blank screen. Hero or any above-the-fold content hidden this way is
the worst case: the first paint is empty.

**First paint must be legible; motion is enhancement, never the thing that makes
content appear.** Project behaviour lives in linked, deferred ES modules that run AFTER the
first paint (see client_js_split: no hand-written `<script src>`, no load-order lever;
an inline classic `<script>` is accepted but runs at parse time, before those modules), so
CSS alone paints that first frame - which means the base rule for a revealed
element must be its VISIBLE state. Never hide hero or first-screen content behind a
scroll/entrance reveal. If you want an entrance, the module ADDS the hidden "prep"
class itself, and only to elements currently below the fold (so above-the-fold
content never flashes), then reveals each on scroll. A module that throws, is slow,
or misses a node then leaves content visible, never blank. Observe any JS-created
nodes AFTER you insert them.

```css
/* WRONG - invisible until JS adds .in; if the reveal never fires, blank forever */
.fade-up{ opacity:0; transform:translateY(20px) }
.fade-up.in{ opacity:1; transform:none }
```
```css
/* RIGHT - visible by default; the module arms the entrance only for off-screen nodes */
.fade-up{ opacity:1; transform:none }
.fade-up.prep{ opacity:0; transform:translateY(20px); transition:opacity .6s, transform .6s }
.fade-up.prep.in{ opacity:1; transform:none }
```
```js
// deferred module: leave in-view nodes visible, arm only what is below the fold
const io = new IntersectionObserver((es, o) => es.forEach(e => {
  if (e.isIntersecting) { e.target.classList.add('in'); o.unobserve(e.target); }
}));
document.querySelectorAll('.fade-up').forEach(el => {
  if (el.getBoundingClientRect().top < innerHeight) return; // in view now: stays visible
  el.classList.add('prep'); io.observe(el);
});
```

## Every togglable surface, one checklist
> when: the page has any panel, drawer, modal, dropdown, tab, accordion or switchable view that a control shows and hides.

Panels, drawers, modals, dialogs, dropdown menus, tabs, accordions, and
app-shaped views are all the same shape: something hidden that a control reveals.
For each, confirm before you move on:

- The element's initial hidden state and the handler's reveal use the **same**
  class or the same mechanism - the handler removes/undoes exactly what the
  initial markup (or CSS) set.
- If a state class reveals it, a CSS rule for that state class actually shows it,
  and there is no always-on hiding class fighting it.
- A close control (an `x`, Esc, a backdrop click, or the toggle again) puts it
  back, using the same convention in reverse.
- A view switch renders the view it shows from current state, not only the
  one that was active when the data changed.
- The control's own state is visible: `aria-expanded` on the toggle,
  `is-active` on the current nav item.
- Nothing covers it: no later sibling or full-bleed layer (`inset:0`) with
  effective `pointer-events:auto` paints above the control or the revealed
  panel; overlays keep `pointer-events:none`, restored per-child.
- Nothing that carries first-screen or hero content starts hidden (`opacity:0`,
  off-screen transform, `visibility:hidden`) waiting on a JS reveal: content is
  legible on first paint with no JS, any entrance animation is gated so its
  un-run state still shows the content, in-viewport elements reveal on load, and
  JS-created nodes are observed after insertion.

## A view switch that survives reload (optional hash sync)
> when: an app-shaped page's active view should survive a refresh or a shared link.

For an app-shaped page whose active view should survive a refresh or a shared
link, sync the view name to the hash - still a client-side switch the server
never sees, never a real route:

```js
function route(){ showView((location.hash.slice(1)) || 'home'); }
window.addEventListener('hashchange', route);
route();
// nav items set location.hash = name instead of calling showView directly.
```

Keep one `showView` as the single place that flips visibility and active state,
so there is exactly one visibility convention to get right.
