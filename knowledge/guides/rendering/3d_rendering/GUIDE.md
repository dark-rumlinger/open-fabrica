---
slug: 3d_rendering
role: capability
title: 3D rendering with Three.js
summary: Three.js/WebGL setup that avoids the null-canvas crash - renderer + mount, resize, DPR, the rAF loop, avoiding the black/NaN screen, and camera-relative movement that doesn't mirror (first-person, third-person follow-cam, orbit); pointer-lock specifics on recall.
category: rendering
position: 30
libraries: [three]
exclusive_with: [phaser_2d]
---

# 3D rendering with Three.js (any WebGL / 3D / first-person page)

## Choose: the pinned three.js entries (copy into `external_libraries` EXACTLY)

Three.js is a `kind:"module"` library. Use the **jsdelivr npm ESM build**: cdnjs does not host the module build at `/three.module.min.js` and 404s. Never substitute a release remembered from elsewhere; this is the platform's verified pin:

```json
{"name":"three.js", "kind":"module", "module_name":"three", "url":"https://cdn.jsdelivr.net/npm/three@0.185.1/build/three.module.js"}
```

For addons (OrbitControls, Sky, GLTFLoader, …) declare a SECOND entry pinned to the SAME version, with a trailing-slash specifier so the subpath prefix resolves:

```json
{"name":"three-addons", "kind":"module", "module_name":"three/addons/", "url":"https://cdn.jsdelivr.net/npm/three@0.185.1/examples/jsm/"}
```

The pair moves together when the pin is raised, or addons fail at runtime.

Then `import * as THREE from 'three';` at the top of **every** file that touches it. Never funnel `THREE` through a shared state object or `ctx` bag: a module-level `new THREE.Vector3()` then runs before `THREE` is assigned and crashes the file at load.

## The null-canvas crash (platform-specific)

The #1 Three.js failure on this platform is `Cannot read properties of null (reading 'width')` thrown from `new THREE.WebGLRenderer` at init - the renderer got a **null canvas**. It happens because page bodies here are **HTML fragments injected into the layout, and your linked module runs at end-of-`<body>`**, so a `<canvas id>` you assumed may not exist yet, or its id doesn't match. Let Three create its own canvas and mount it - the robust default, no id to get wrong:

```js
const renderer = new THREE.WebGLRenderer({ antialias: true });
const mount = document.getElementById('game');   // the <div id="game"> your page fragment declares
mount.appendChild(renderer.domElement);                          // renderer.domElement IS the canvas
```

Run init from the linked module (which loads after the fragment is in the DOM), not from an inline `<script>` in the layout. Don't ALSO hand-write a `<canvas>` and pass it in - pick one path.

**One `WebGLRenderer` per page - never one per thumbnail.** Browsers cap live WebGL contexts at roughly 8-16, and past that three.js dies in `getUniforms`/`onFirstUse` with null-shader errors whose stack points nowhere near the cause. Render previews with ONE shared offscreen renderer and copy each result onto a 2D canvas per tile (`drawImage`).

## Mounting gives you a canvas, not a picture - you must DRAW every frame

`renderer.render(scene, camera)` is a **one-shot draw of the current state, not a subscription**: nothing reaches the screen until you call it, and it must run EVERY frame. Own that call:

```js
const clock = new THREE.Clock();
function frame() {
  const dt = Math.min(0.05, clock.getDelta());  // clamp: a backgrounded tab wakes with a huge first delta
  update(dt);                                   // move things...
  renderer.render(scene, camera);               // ...then PAINT them - without this line the canvas is black forever
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);
```

`renderer.setAnimationLoop(frame)` is the native equivalent (required for WebXR); with it, drop the trailing `requestAnimationFrame`.

**The trap is a loop that runs, moves the camera, and never calls `render`**: nothing throws and the user gets pure black. An update-callback registry (`runUpdates(dt)`) invites this, because the draw is not an update. Call `render` explicitly at the end of the frame, and if the canvas is black confirm it runs before debugging anything else.

**Sizing: cap `devicePixelRatio` at 2, and on resize re-`setSize`, set `camera.aspect` and call `camera.updateProjectionMatrix()`** (the aspect change does nothing until it runs).

## A stray NaN blacks the WHOLE canvas - and throws nothing, so the load probe passes

If the canvas is black and you have confirmed you ARE drawing every frame (above), the next cause is a `NaN`.

One `NaN` in `camera.position` or rotation makes every vertex `NaN` and the renderer paints pure black with nothing logged. The usual source is an **optional per-frame argument used unguarded**: `update(dt, mouseDX, mouseDY)` during play but `update(dt)` from a splash or pause frame, so `yaw -= undefined * k` is `NaN`. Default the optionals at the top (and clamp anything fed in from physics or network):

```js
function update(dt, mdx, mdy) {
  mdx = mdx || 0; mdy = mdy || 0;          // a no-arg call must not poison yaw/pitch
  yaw   -= mdx * 0.0025;
  pitch -= mdy * 0.0025;
  if (!Number.isFinite(yaw))   yaw = 0;    // belt-and-braces against any NaN that slipped in
  if (!Number.isFinite(pitch)) pitch = 0;
}
```

**`NaN` is sticky:** once `pos`/`yaw` is `NaN`, every `+=` / `.lerp()` keeps it `NaN`, so the scene never recovers. The tell is a black screen with `NaN` coordinates in the HUD. Guard the inputs; a `try/catch` does not help, since `NaN` is valid arithmetic and never throws.

## Movement must resolve against the camera you render - or it mirrors
> Applies to EVERY movable-view scheme: first-person, third-person follow-cam, orbit - not just FPS.

The most common silent 3D defect is **mirrored controls**: after you turn, W walks backward or D strafes the wrong way, and nothing throws. The cause is **two copies of the yaw trig** (one orients the camera, one drives movement) disagreeing on a sign. Keep ONE source of truth: derive the movement basis from the camera you render, never from a second hand-written `(sin, cos)` pair.

```js
// Works for ANY camera - FPS eye, third-person follow-cam, orbit rig. matrixWorld must be current this frame.
camera.updateMatrixWorld();
const fwd = new THREE.Vector3(), right = new THREE.Vector3();
camera.getWorldDirection(fwd);  fwd.y = 0;  fwd.normalize();               // flatten so W walks, doesn't fly
right.setFromMatrixColumn(camera.matrixWorld, 0);  right.y = 0;  right.normalize();  // the camera's own +X (right)
const moveF = (held.KeyW ? 1 : 0) - (held.KeyS ? 1 : 0);
const moveS = (held.KeyD ? 1 : 0) - (held.KeyA ? 1 : 0);
vel.x = (fwd.x * moveF + right.x * moveS) * speed;   // a straight-down view flattens fwd to zero -> NaN (see above);
vel.z = (fwd.z * moveF + right.z * moveS) * speed;   // clamp pitch away from vertical so normalize() stays finite
```

**Third-person / follow-cam is where this breaks most.** The camera is placed around the avatar each frame (`camera.position = avatar + offset(yaw); camera.lookAt(avatar)`), and one flipped sign between that offset and a hand-written move pair inverts every control while spawn still looks fine. Place the camera however you like, then read the basis back off it with the block above (`getWorldDirection` AFTER `lookAt`). If you hand-roll both, derive them from ONE yaw and assert once at spawn that `f.x*fwd.x + f.z*fwd.z > 0`, so a mirrored build fails the load probe instead of shipping.

## Reach past the lit primitive

One lit box on a plane reads as a prototype, and the cheap ways past it are all procedural, so no assets are needed:
- Build each character, vehicle or prop as a `THREE.Group` of 5 to 20 primitives with a readable silhouette, sharing geometries and materials.
- Light with intent: a hemisphere fill, one shadow-casting directional light with tight `shadow.camera` bounds, a colored accent light, ACES filmic tone mapping, and `scene.fog` matched to the sky.
- Texture the world without images: vertex-color or noise-shaded ground, scattered `InstancedMesh` props, a gradient or `Sky` backdrop, ambient motion.
- Add a post pass (such as `UnrealBloomPass`) only where the look earns it, and answer every action with feedback (see `game_feel`).

Do this once the loop, controls and win/lose path work; an unplayable scene still fails.

<!-- extended -->

## Pointer-lock first-person controls
> when: building a first-person / pointer-lock camera - WASD + mouse-look, or any FPS-style scheme.

Request pointer lock from a **user gesture** (`domElement.addEventListener('click', () => domElement.requestPointerLock())`), track it via `pointerlockchange` (show a "click to play" overlay when unlocked - the browser releases lock on Escape), and read mouse **deltas** (`e.movementX/Y`), never absolute coords: `yaw -= movementX * k; pitch -= movementY * k`, then **clamp pitch to just inside +/-PI/2 so you can't flip over**. WASD: track held keys in `keydown`/`keyup` and apply them in `update(dt)` - never move straight from the `keydown` event (that's one step per OS key-repeat, not smooth motion).

Resolve movement against the camera per the **core rule above** (`getWorldDirection`), so mouse-look and WASD can never disagree. One FPS-only shortcut: if you already drive the camera from a single `yaw` (`camera.rotation.order = 'YXZ'`, `camera.rotation.y = yaw`, mouse `yaw -= movementX`), you may build the basis from that same `yaw` instead of reading it back - but do NOT introduce a third convention, use exactly this vetted pair:

```js
const forward = { x: -Math.sin(yaw), z: -Math.cos(yaw) };   // W / S
const right   = { x:  Math.cos(yaw), z: -Math.sin(yaw) };   // D / A
```

## Voxel / block worlds
> when: routing a blocky vs smooth world - the brief mentions cubes/blocks/Minecraft, or you must decide voxel vs heightfield terrain.

Blocky-vs-smooth routing is decided at plan time: a world of discrete breakable cubes is the **`voxel_worlds`** guide, any smooth/organic/planetary landscape is **`heightfield_terrain`** - each carries its own boundary rule.
