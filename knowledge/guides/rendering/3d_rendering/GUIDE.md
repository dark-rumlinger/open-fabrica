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

Three.js is `kind:"module"` via **jsdelivr npm ESM** (cdnjs lacks `/three.module.min.js`):

```json
{"name":"three.js", "kind":"module", "module_name":"three", "url":"https://cdn.jsdelivr.net/npm/three@0.185.1/build/three.module.js"}
```

If using addons (`OrbitControls`, `Sky`, `GLTFLoader`, ...), add a second entry pinned to the **exact same release**:

```json
{"name":"three-addons", "kind":"module", "module_name":"three/addons/", "url":"https://cdn.jsdelivr.net/npm/three@0.185.1/examples/jsm/"}
```

`import * as THREE from 'three';` at the top of every file that uses it — never pass `THREE` through `ctx` or a store object.

## Platform mount & silent black-screen traps

- **Mount into a fragment container (avoids null-canvas crash):** pages are HTML fragments injected into the layout, and linked modules run after DOM ready. Let Three create its own canvas (`const renderer = new THREE.WebGLRenderer({ antialias: true }); document.getElementById('game').appendChild(renderer.domElement);`) from the linked module (never an inline `<script>`). Use **one `WebGLRenderer` per page** (use a shared offscreen renderer + `drawImage` to 2D canvases for thumbnails).
- **Render every frame & resize:** call `renderer.render(scene, camera)` at the end of `requestAnimationFrame` (or `setAnimationLoop`), clamp `dt <= 0.05`, cap DPR at `2`, and call `camera.updateProjectionMatrix()` on resize.
- **Guard against `NaN`:** one `NaN` in camera position/rotation blacks out the scene without throwing. Default optional `update(dt, mdx = 0, mdy = 0)` args and guard with `Number.isFinite`.
- **Derive movement from the rendered camera:** never maintain a second hand-written `(sin, cos)` yaw basis (causes mirrored WASD). After placing/rotating the camera, read `fwd` via `camera.getWorldDirection(fwd)` (flattened `y = 0`, normalized — clamp pitch away from ±PI/2 so `normalize()` doesn't `NaN`) and `right.setFromMatrixColumn(camera.matrixWorld, 0)`.
- **Richness:** build props/characters as `THREE.Group`s (not single primitives), use hemisphere + shadow-casting directional light + fog/ACES tone mapping, and pair with `game_feel`.

<!-- extended -->

## Pointer-lock first-person controls
> when: building a first-person / pointer-lock camera - WASD + mouse-look, or any FPS-style scheme.

Request pointer lock on click (`domElement.requestPointerLock()`), track `pointerlockchange` (show a "click to play" overlay when unlocked), accumulate `e.movementX/Y` deltas, and clamp pitch inside `±PI/2`. Track held keys via `keydown`/`keyup` and apply in `update(dt)` using the camera's `getWorldDirection` basis (or for `camera.rotation.order = 'YXZ'`, `forward = {-sin(yaw), -cos(yaw)}`, `right = {cos(yaw), -sin(yaw)}`).

## Voxel / block worlds
> when: routing a blocky vs smooth world - the brief mentions cubes/blocks/Minecraft, or you must decide voxel vs heightfield terrain.

Discrete breakable cubes use **`voxel_worlds`**; smooth/organic/planetary terrain uses **`heightfield_terrain`**.
