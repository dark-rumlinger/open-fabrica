---
slug: auth_contract
role: capability
title: Authentication
summary: The auth endpoint contract plus the platform-owned browser bootstrap (token drain, requireRole, apiFetch).
category: security
position: 20
tools: [manage_auth, manage_oauth, manage_2fa, manage_roles, manage_tenancy]
---

# Auth-page contract (any project with create_auth / create_oauth)

**Table prerequisite - build this BEFORE `create_auth`.** `create_auth` runs over a user table you create first, and that table MUST already have:

- a **unique login column** - whatever you pass as `username_column` (required; commonly `email` or `username`). `create_auth` does NOT verify uniqueness, so declare it `unique` at `create_table`; and
- a **`password` column of type `PASSWORD`** - the platform hashes it on write. A plain `TEXT` `password` is rejected (`column "password" ... is not a PASSWORD column`), forcing a wasteful alter-and-retry.

Create the table in that final shape from the start - don't scaffold it bare and `alter_table` the password column in later. Add a `role` column (with a default) if you gate by role. Do NOT add a `user_id` foreign key to `of_users`: that is the platform's GLOBAL admin table, not a project table, so the reference is dangling. (Passwordless email login is available by layering `manage_2fa` `magic_link` on top of this endpoint - but the base `/register`+`/login` are password-based, so the `PASSWORD` column is still required.)

After `create_auth`, the platform emits the auth bootstrap `js/auth.js` (a global-scope ES module, specifier `'auth'`, with this endpoint's path baked in) as a platform-managed file - do NOT author, recreate, or delete it. It EXPORTS the helpers every page imports - `import { requireRole, requireAuth, redirectIfLoggedIn, apiFetch, logout, user, authReady } from 'auth'` - pages import those, never fetching `/me` or writing their own guard functions. (`user` is a live-binding export - null until the bootstrap's `/me` resolves - and `authReady` is that Promise.)

The login/register response shape, the `auth_token` storage key and the `apiFetch`
rules are the **auth bootstrap** law in your mental model, which rides this prompt
already. Platform-specific elaboration on top of it:

- **Reading user fields after login.** Do NOT read `response.user.*` (e.g. `data.user.username`) - there is no `user` object, so a redirect built from it silently breaks. When the destination depends on a user field (a per-user URL like `/<username>/dashboard`), redirect to a **fixed** path and let that page read the field from the `/me` row via `requireAuth().then(function (u) { ... u.username ... })` (imported from `'auth'`), or reuse the value the form already submitted.
- **`requireRole` semantics.** It resolves to the user, or `null` after it has already redirected (unauthenticated -> `/login`; authenticated-but-wrong-role -> `/`). The resolved value is the `/me` row; `requireRole(role)` passes when `user.role` is that role or `admin` (admin passes every role gate, as on the server), so render from whatever other columns `/me` returns. Synchronous self-written guards race the async bootstrap and produce the admin <-> login flash loop.
- **What `/me` returns / `public_columns`.** By default `/me` returns `id`, the username column and the role column. Setting `create_auth`'s `public_columns` REPLACES that set (list `id` and the username column too if pages read them); only the **role column is always added** so `requireRole` can read `user.role`. That argument is a **bare array of column-name strings** - `["id","username","role","display_name"]`, never objects like `{"name":"id"}`. To show another user field on a protected page (display name, avatar URL), add its column there; the PASSWORD column is never returned.
- **`/login` page: call `redirectIfLoggedIn('/admin')` at the TOP of the page's linked script module** (`import { redirectIfLoggedIn } from 'auth'` - an inline `<script>` cannot import, so the login page's JS is a linked `kind=file`). It awaits the bootstrap's single `/me` check and bounces only a *validated* user (a stale token is dropped, so the visitor stays to sign in). Fire-and-forget is fine. Do not write your own `/me` call on the login page.
- **Which screens are pre-auth.** The `default` layout IS the authenticated chrome (sidebar, nav, signed-in identity), so rendering it around a login form leaks the dashboard before sign-in and flashes nav at an unauthenticated visitor. `/login`, `/signup` and `/reset` are pre-auth screens and escape the shell the way your mental model's page/layout law describes. An in-app *change-password* page, behind `requireAuth`, is NOT one - it stays in the shell.
- **Login authenticates anyone valid; gate roles in TWO places.** `POST /api/<auth>/login` issues a JWT to ANY user whose credentials are correct - it does not restrict *who* may sign in by role. Gate the UI per-page with `await requireRole('admin')` (imported from `'auth'`), which bounces a wrong-role user to `/`. But a page guard is UX only - it does NOT protect the data: a signed-in member can still call the backing `/api/...` endpoint directly. Gate every admin/privileged DATA endpoint server-side too - `manage_api` `auth_model=role` with `required_role` (roles themselves come from `manage_roles`), or an owner/tenant scope - or the endpoint leaks to any authenticated user. Describe the page gate as "non-admins are bounced from admin pages," never "non-admins cannot authenticate"; refusing login itself for a role is a separate, explicit concern, do not assume `create_auth` does it.
- **`apiFetch` path rule.** A `path` starting with `/` is used verbatim, anything else gets an `/api/` prefix - so `apiFetch('users')` and `apiFetch('/api/users')` both hit `/api/users`, but `apiFetch('api/users')` double-prefixes to `/api/api/users` and 404s. A plain-object or array `opts.body` is stringified with `Content-Type: application/json` for you; `FormData`, `Blob`/`File` and `URLSearchParams` pass through untouched.
- **Unwrapping `apiFetch`.** That it returns the raw `Response` (not parsed JSON) and how you read `.rows` off `.json()` is the auth bootstrap law in your mental model - don't re-derive it here; see `api_response_contract` for the envelope-vs-bare-row split.
- **OAuth and magic-link are automatic; register is not.** The bootstrap drains `#auth_token=<jwt>` from `location.hash` on first paint, stores it, and `replaceState`s clean (OAuth callback and magic-link verify only). `POST /register` answers JSON `{success, user_id, token}`: store `data.token` yourself.

<!-- extended -->

## Login step-ups (2FA / tenancy)
> when: the project enables manage_2fa or manage_tenancy and the login flow must handle a tokenless 200.

The contract's "gate on `success`" rule cashes out as four shapes, each only on a project that enabled that feature. All are `200` with `success: false` and no token, so only `if (!data.success)` catches them - `res.ok` is true and a `try/catch` never fires:

- `needs_totp: true` (a `manage_2fa` project) - re-POST the same credentials plus `totp_code`. Only users who ENROLLED are challenged. Enrollment is `POST /api/<auth>/totp/setup` (returns `secret`/`otpauth_url`/`qr_url`), then `POST /api/<auth>/totp/verify {code}` once, both with the user's session token.
- `needs_totp_setup: true, enroll_token` (a `manage_2fa` endpoint created with `enforced: true`, an account that has not enrolled) - there is no session yet. Send `setup` and then `verify {code}` with `Authorization: Bearer <enroll_token>` (a 15-minute, single-use token that works for those two routes only); the successful `verify` response IS the sign-in (`{success, user_id, token, enrolled: true}`), so store its `token` like a login's. Without `enforced`, an unenrolled user logs in with the password alone. `manage_2fa disable` stops every challenge.
- `needs_tenant_select: true, tenants: [...]` (a `manage_tenancy` user in several tenants) - render a chooser, then re-POST **login** with `?tenant=<slug>` from the list. Do NOT reach for `/tenant/switch` here: it re-mints a token you already hold, so it 401s for a caller whose login just declined to issue one. `/tenant/switch` is for changing tenant *later*, from inside the app.
- `needs_tenant: true, signup_mode: "invite_only"|"signup_creates_tenant"|"signup_joins_default"` - the user has no active tenant. Show what `signup_mode` allows; `invite_only` means they wait for an invite, so say that rather than offering a dead-end form.

**Joining by invite.** An invitee registers with `POST /api/<auth>/register?invite=<token>`; the invite is single-use, expires, and its email must equal the username the visitor registers with. `manage_tenancy enable` starts a fresh endpoint in `invite_only`, so a plain `/register` answers 403 until you pick a mode with `manage_tenancy set_signup_mode`.

**OAuth and magic-link sign-ins pass the same TOTP gate.** When the user enrolled (or the endpoint is `enforced` and they have not), the sign-in issues no session: it redirects to **`/login#totp_verify=<token>`** (enrolled) or **`/login#totp_setup=<token>`** (enroll first); a JSON magic-link verify answers `needs_totp_verify: true, totp_token` or the `needs_totp_setup` shape above. The `/login` page reads that fragment on load, shows the code field (after `setup`'s QR for `totp_setup`), and POSTs `verify {code}` with `Authorization: Bearer <token>` (single use, 10 to 15 minutes); the `verify` response is the sign-in, so store its `token` and navigate. These sign-ins skip tenant selection (the token carries no `tenant_id` until the client calls `/tenant/switch`) and auto-create a user on first sign-in.

## What `/register` accepts
> when: the user table has fields beyond username and password (profile fields, plan, verified flags).

`/register` takes the username, the password and any extra columns named in `create_auth`'s `signup_columns` (a bare array of column names, e.g. `["display_name"]`); with it set, every other body field is dropped. Without it any table column is accepted except privilege-like ones (`is_admin`, `verified`, `credits`, `plan`, ...), and the role is always `default_role`. Set `signup_columns` whenever the table carries standing fields a visitor must not choose.

## Password reset
> when: the plan has a "forgot password" link or a `/reset` page.

The platform serves it; build only the two screens. `POST /api/<auth>/password/forgot` with `{<username column>: "<email>"}` (the username column must hold an email address) mails a one-hour, single-use link to **`/reset?token=<token>`** on the project's own domain, so the `/reset` page reads `token` from the query string and POSTs `{token, password}` to `POST /api/<auth>/password/reset`, then sends the user to `/login`. Both always answer a generic 200 for an unknown address; a wrong or used token is 401. It needs an email provider configured with `manage_email`, and a project domain (or localhost while testing) for the link's origin. A successful reset signs out every existing session of that user and clears any login lockout, so send them to `/login`. To brand the email, save a `manage_email` template named `password_reset` with the link at `{{reset_url}}`; without one a plain default is sent.

## OAuth provider configuration
> when: the plan includes create_oauth.

A `create_oauth` task has two halves. At BUILD: the auth endpoint plus a sign-in button linking to `/api/<auth>/oauth/<provider>` (it answers "provider not configured" until registered). `manage_oauth create` requires `provider_name`, `client_id`, `client_secret`, `authorize_url`, `token_url`, `userinfo_url` and `auth_path` (there are no provider presets, so the three URLs come from the provider's docs); `provider_name` is unique project-wide, a repeat reports `already_existed`. The redirect URI to register in the provider's console is `https://<domain>/api/<auth>/oauth/<provider>/callback`. The `client_id`/`client_secret` come from the provider's console, which only the owner has: call it during the build only if the owner already gave them, never with placeholder values; otherwise it runs later in owner chat once they supply them (`manage_oauth update` replaces credentials on a provider already registered).

## How the bootstrap works (platform-owned - you do not author it)
> when: you want to know why auth never flashes, or are debugging the bootstrap's behaviour.

The platform emits `js/auth.js` (with `__AUTH_PATH__` replaced by this endpoint's auth path) as a platform-managed global ES module when you run `create_auth`; the model cannot edit or delete it. The guarantees it bakes in are exactly what keep auth from flashing: the fragment drain runs at module evaluation, the exported `authReady` is a single in-flight `/me` promise (the module evaluates once), and a 401/403 from `/me` drops the stored token while a network error does NOT (transient offline must not log the user out). Pages `import { requireRole } from 'auth'` and `await requireRole(...)`, never read a synchronous global.
