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

**One visibility convention per element: reveal by undoing exactly what hid it.** Mixing a hiding class (`.hidden`) with a state class (`.open`) or the `[hidden]` attribute leaves the element `display: none` when clicked.

- **Class hide:** `.hidden { display: none }` -> `el.classList.toggle('hidden')`.
- **State class:** `#drawer { display: none } #drawer.open { display: flex }` -> `el.classList.toggle('open')` (never also put `.hidden` on `#drawer`).
- **Boolean attribute:** `[hidden]` -> `el.hidden = false` / `el.removeAttribute('hidden')`.

**Switching client-side views:** use the same convention, mark the active nav item, and call the view's renderer on every switch so hidden views never go stale:

```js
const renderers = { home: renderHome, list: renderList };
function showView(name) {
  document.querySelectorAll('[data-view]').forEach(v => v.classList.toggle('hidden', v.dataset.view !== name));
  document.querySelectorAll('[data-nav]').forEach(n => n.classList.toggle('is-active', n.dataset.nav === name));
  renderers[name]?.();
}
```

**Two silent hit-testing / paint traps:**
- Full-viewport layers (`inset: 0`, touch controls, vignettes, closed overlays) must keep `pointer-events: none` and restore `auto` only on their own interactive children — never `#parent > * { pointer-events: auto }`. See `nothing-may-sit-on-top-of-a-control`.
- Above-the-fold content must be visible by default without JS (`opacity: 1`); never hide hero/first-screen elements waiting for an entrance class. See `entrance-and-scroll-in-animations`.

<!-- extended -->

## Nothing may sit on top of a control
> when: a full-viewport or absolutely-positioned layer (touch controls, a joystick zone, a vignette, an overlay or modal backdrop) sits over interactive UI.

A full-viewport layer (`position: absolute`/`fixed`, `inset: 0`) painted after interactive UI eats every click unless it has `pointer-events: none` with `pointer-events: auto` restored only on its own interactive children. Never use `#parent > * { pointer-events: auto }` — ID specificity outranks a `.overlay { pointer-events: none }` class rule and turns the layer into an invisible click shield.

## Entrance and scroll-in animations
> when: content animates in on load or on scroll (an opacity/transform/visibility reveal driven by a JS-added class).

Project JS runs in deferred ES modules after first paint, so the base CSS rule for any animated element must be its **visible** state (`opacity: 1; transform: none`). Add a `.prep` hiding class from JS only to elements currently below the fold (`getBoundingClientRect().top >= innerHeight`), then reveal them with `IntersectionObserver` (and observe any JS-inserted nodes after insertion).

## View switch with hash sync
> when: an app-shaped page's active view should survive a refresh or a shared link.

```js
function route() { showView(location.hash.slice(1) || 'home'); }
window.addEventListener('hashchange', route);
route();
```
