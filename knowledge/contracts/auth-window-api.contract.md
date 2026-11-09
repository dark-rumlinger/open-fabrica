---
id: auth-window-api
title: Auth token key, login response, and helper module (import from 'auth')
kind: kv
governs: []
probes: [auth_token]
---
**Auth contract (any `create_auth` / `create_oauth` project).** The platform emits the global bootstrap `js/auth.js` as an ES module (platform-managed - never author, recreate, or delete it). Import its helpers by specifier (`import { requireRole, apiFetch } from 'auth'`); never write your own guards or fetch `/me` directly.

- **Token storage key is exactly `auth_token`.** Your `/login` submit handler does `localStorage.setItem('auth_token', data.token)` then one full navigation (`location.href = '/admin'`), never `location.hash`.
- **A SUCCESSFUL login/register response is exactly** `{ "success": true, "user_id": <int>, "token": "<jwt>" }` - there is NO `user` object on it. Store `token`, navigate, done.
- **Gate that store on `success`, never on the HTTP status.** A login needing one more step (2FA code, tenant choice) answers **200 with `success: false` and NO token**; `localStorage.setItem('auth_token', data.token)` on any 200 stores `undefined`, the login looks like it worked, and every later call 401s. `if (!data.success)` first - the `auth_contract` guide carries the step-up shapes and how to finish each one.
- **Protected pages:** `import { requireRole, requireAuth, authReady } from 'auth'` then `await requireRole('admin')` (or `requireAuth()`; `admin` passes every role gate). Never read the imported `user` synchronously at module-eval time (it is null until `authReady` resolves) or gate on `if (authReady)` (a Promise is always truthy); use `authReady.then(...)`.
- **Every API call goes through `apiFetch(path, opts)`** (imported from `'auth'`; adds the Bearer header, redirects 401 -> `/login`). `logout()` drops the token in THIS browser only (no server logout; the token stays valid until it expires).
- **`apiFetch` resolves to the raw `Response`, NOT parsed JSON.** It adds the Bearer header, maps a bare path to `/api/<path>`, JSON-encodes a plain-object `body` (pass the object, not a `JSON.stringify` string, or no Content-Type is sent) and 401-redirects - it does NOT call `.json()` for you, so you `await` twice: `const { rows } = await (await apiFetch('users')).json();`. Reading `.rows`/`.count` off the `apiFetch` result directly (without `.json()`) yields `undefined` - a list renders empty. The body shape itself - the LIST envelope vs the bare row - is the REST response law above; unwrap per that, and never destructure `.data` a second time.
