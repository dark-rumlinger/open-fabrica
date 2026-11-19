---
slug: auth_contract
role: capability
title: Authentication
summary: The auth endpoint contract plus the platform-owned browser bootstrap (token drain, requireRole, apiFetch).
category: security
position: 20
tools: [manage_auth, manage_oauth, manage_2fa, manage_roles, manage_tenancy]
---

# Auth-page contract (`create_auth` / `create_oauth`)

**User table prerequisites (create BEFORE `create_auth`):**
- A `unique` login column (`username_column`, e.g. `email` or `username`).
- A `password` column of type `PASSWORD` (not `TEXT` — auto-hashed on write).
- Optional `role` column with a default for role gating. Never FK `user_id` to `of_users` (that is the global platform admin table).

**Platform-managed `'auth'` module (`js/auth.js`):** auto-emitted on `create_auth` (do not create, edit, or delete). Import from `'auth'`:
`import { requireRole, requireAuth, redirectIfLoggedIn, apiFetch, logout, user, authReady } from 'auth'`

- **No `response.user` on login:** read user fields on protected pages from `await requireAuth()` (`/me` row), or redirect to a fixed path.
- **`requireRole(role)`:** resolves to `/me` row (allows `role` or `admin`) or `null` after redirecting (unauthed → `/login`, wrong role → `/`). Always await `'auth'` helpers rather than writing synchronous guards.
- **`public_columns` on `create_auth`:** bare string array (`["id","username","role","display_name"]`, never objects). Setting it replaces the default (`id`, username); `role` is always included and `PASSWORD` is always omitted.
- **`/login` page:** call `redirectIfLoggedIn('/admin')` at the top of a linked `kind=file` module (no inline `<script>`). Pre-auth screens (`/login`, `/signup`, `/reset`) use `metadata.layout: "none"`; in-app password change keeps the shell behind `requireAuth`.
- **Gate in TWO places:** `requireRole` is UX only; gate privileged data endpoints server-side via `manage_api` (`auth_model=role` + `required_role`, or `owner`/`tenant`). Describe page gates as "non-admins are bounced from admin pages", not "cannot authenticate".
- **`apiFetch(path, opts)`:** paths starting with `/` are used verbatim; others get `/api/` prepended (never pass `'api/users'`). Plain-object/array `opts.body` is JSON-stringified automatically; `FormData`/`Blob`/`URLSearchParams` pass through untouched. Returns raw `Response` (see `api_response_contract`).
- **Token handling:** OAuth and magic-link drain `#auth_token=<jwt>` automatically; `POST /register` returns `{success, user_id, token}` — store `data.token` yourself.

<!-- extended -->

## Login step-ups (2FA / tenancy)
> when: the project enables `manage_2fa` or `manage_tenancy` and login returns HTTP 200 with `success: false` and no token.

Always gate on `if (!data.success)` (200 responses don't trigger `!res.ok` or `catch`):
- `needs_totp: true` (`manage_2fa`): re-POST credentials + `totp_code`. Enrollment uses `POST /api/<auth>/totp/setup` then `POST /api/<auth>/totp/verify {code}` with the user's session token.
- `needs_totp_setup: true, enroll_token` (`manage_2fa` `enforced: true`): call `setup` then `verify {code}` with `Authorization: Bearer <enroll_token>` (15-min single-use); `verify` returns `{success, user_id, token, enrolled: true}`.
- `needs_tenant_select: true, tenants: [...]`: render a chooser and re-POST login with `?tenant=<slug>` (never `/tenant/switch`, which requires an existing session).
- `needs_tenant: true, signup_mode`: show message per mode (`invite_only`, `signup_creates_tenant`, `signup_joins_default`). Invites register via `POST /api/<auth>/register?invite=<token>` (email must match username). `manage_tenancy enable` starts in `invite_only`; change with `manage_tenancy action=set_signup_mode`.
- OAuth / magic-link TOTP challenges redirect to `/login#totp_verify=<token>` or `/login#totp_setup=<token>` (magic-link JSON returns `needs_totp_verify`/`needs_totp_setup`); POST `verify {code}` with `Authorization: Bearer <token>`.

## Registration (`signup_columns`)
> when: the user table has fields beyond username and password.

Set `signup_columns` (bare array, e.g. `["display_name"]`) on `create_auth` whenever the table has fields visitors must not self-assign (without it, non-privilege columns are accepted).

## Password reset
> when: the plan has a "forgot password" link or `/reset` page.

Requires `manage_email` configured. `POST /api/<auth>/password/forgot` with `{<username_column>: "<email>"}` sends a 1h link to `/reset?token=<token>`. `/reset` POSTs `{token, password}` to `POST /api/<auth>/password/reset` and redirects to `/login`. Optional `manage_email` template `password_reset` uses `{{reset_url}}`.

## OAuth provider configuration
> when: the plan includes `create_oauth`.

`manage_oauth create` needs `provider_name` (unique project-wide), `client_id`, `client_secret`, `authorize_url`, `token_url`, `userinfo_url`, and `auth_path`. Redirect URI: `https://<domain>/api/<auth>/oauth/<provider>/callback`. Sign-in links hit `/api/<auth>/oauth/<provider>`. Never pass placeholder secrets at build time; configure in owner chat via `manage_oauth create`/`update` once the owner supplies credentials.
