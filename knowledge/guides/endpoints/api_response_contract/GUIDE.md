---
slug: api_response_contract
role: capability
title: API response shapes
summary: Using a create_api endpoint from the client - paging, ?q= search, filters, row addressing, and scoping a user-owned table (owner vs authored vs public). Reach for it whenever a page reads or writes a CRUD endpoint.
category: endpoints
position: 25
tools: [manage_api]
---

# REST API response shapes (any project with create_api)

The envelope itself (parse with `.json()`, then read `.rows`; `apiFetch` returns the raw
`Response` and does not unwrap) is the **REST response shapes** law in your mental model -
already on this prompt. This guide adds what the law does not
carry: querying, addressing, and row ownership.

- **Paging.** Page with `?limit=&offset=` (limit max 200, a larger one becomes 50) until a short page (`rows.length < limit`) comes back.
- **Search.** `?q=term` substring-matches the endpoint's readable TEXT columns;
  semantic search is a different capability.
- **Scope before you ship.** A table whose rows belong to a user (`user_id`/`owner_id`/
  `created_by`) MUST get `auth_model=owner` (private) or `authored` (shared reads, owned
  writes), tenant scoping for a workspace (`manage_tenancy add_tenant_column`), or a
  deliberate `auth_model=public` / `public_read` - the plan's data-ownership decision
  names which, and `manage_api`'s own notes carry each choice. The build-end `rls_scope` gate is the backstop.

The sections below are the full detail of LIST filters, row addressing (`lookup_column`),
and each ownership choice - recall one when you reach its case.

<!-- extended -->

## Query a LIST: filter, sort, and full-text search
> when: a list view filters, sorts or searches rows (or you are setting searchable_columns).


A LIST endpoint takes query params with no extra endpoint code: `?col=value` (exact match), `?col__gt`/`__gte`/`__lt`/`__lte`/`__ne=` (comparisons), `?col__like=` (pattern match), `?col__has=` (one whole token of a comma-separated column), `?sort=col&order=asc|desc`, `?fields=a,b` (return only those columns), and `?limit=&offset=` (paging, above). Aggregates: `GET /api/<path>/_stats?fn=&column=&group_by=`. For keyword search over text, query **`?q=term`** - a substring match across the endpoint's readable TEXT columns (`public_columns` narrows them). It is a LIKE scan, so for a large table use the FTS5 index (`searchable_columns` at `create_table`, or `manage_keyword_search`) through `manage_data action=search` instead. Semantic / "find similar" search is a *different* capability - an embeddings provider plus `manage_vector_search`, or retrieval-augmented answers - covered by the `llm_endpoints` guide, not `?q=`.

## Column exposure: `public_columns` and `writable_columns`
> when: a table has fields clients must not read or set (status, role, paid, credits, internal notes).

`public_columns` bounds what a client can read, filter, sort, search and aggregate on; a filter or sort on a hidden column is a 400, not ignored. `writable_columns` bounds what a client can set on POST/PUT: list only the fields a form edits and leave privileged fields out, so they change only through a trigger or function. Without it every column of the table is client-writable.

## Addressing rows: numeric `id` vs. string lookup
> when: a detail page or share link addresses a row by a string (slug, code, share id), or the table has several UNIQUE TEXT columns.


A single row is addressed by appending the value to the collection path -
`GET /api/posts/42` (numeric -> `id`) or `GET /api/posts/hello-world` (string ->
the table's `slug`/`uuid`, else its sole non-id UNIQUE TEXT column). When the public
identifier has a domain name (e.g. `rooms.code`, `posts.share_id`) or the table has
more than one UNIQUE TEXT column, set `lookup_column` explicitly on `create_api` so
string lookups resolve to the column you mean. (An `endpoint_path` is literal - the
`:param` route syntax belongs to PAGE routes only, per the routing contract in your
mental model; a `:param` or `{param}` in an endpoint path is refused with a bad-arg
error, no endpoint is created.)

## Row ownership: scope owner-owned tables or leak every user's data
> when: a table's rows belong to a user or workspace, or anonymous visitors submit rows (a contact form) that only an admin reads.


When a table's rows belong to one user (it carries a `user_id`/`owner_id`/
`created_by`/`author_id`-style column), the API over it MUST be scoped or every
signed-in user reads AND writes everyone else's rows - a cross-user (IDOR) leak
that still "runs" green. Pick one on `create_api`:

- **`owner_column`** (`auth_model=owner`) - PRIVATE per user: every request is
  filtered to the caller's own rows (`WHERE <owner_column> = <caller id>`), and
  inserts stamp it automatically. Requires a login (`create_auth`).
- **`auth_model=authored`** (also needs `owner_column`) - SHARED reads, owned
  writes: every signed-in user GETs the whole list, while PUT/DELETE reach only the
  rows they authored and POST stamps the author. This is the pick for collaborative
  content grouped by a channel/board/room - messages, cards, comments. Reaching for
  `owner` there hides every other member's rows and the shared view is silently
  empty. Needs an auth surface exactly like `owner`.
- **Tenant scoping** (`manage_tenancy action=add_tenant_column`, not a `manage_api` arg) -
  rows shared within a workspace/team, isolated across tenants.
- **`public_read`** - deliberately world-readable (a public blog feed, a shared
  leaderboard). Choose this *explicitly* when all-rows-visible is the intent.
- **`public_write`** - the inverse of `public_read`, for the *public-submission*
  shape: anyone may POST but only an authed admin reads (a contact form, feedback,
  a waitlist, an error report). Set `auth_model=authenticated` + `public_write=true`
  so writes are anonymous while GET stays gated. Do **not** reach for
  `auth_model=public` here - that also exposes the reads, leaking every submitted
  name/email/message. `public_write` is invalid with `owner_column` (an anonymous
  writer has no identity to own the row).

`create_api` **auto-adopts** an owner-like column as `owner_column` for an
authenticated, non-public endpoint that you left unscoped, and returns
`owner_scope_applied` telling you so - if the rows are one shared list members
co-read, switch it with ONE `update` call carrying `auth_model=authored owner_column=<col>`
(the update rewrites every auth column, so re-send `public_read` if you had set it); if they are genuinely public, say so with
`auth_model=public` (or `public_read`). The
build-end `rls_scope` gate is the backstop; scope it at create time and it never fires.
