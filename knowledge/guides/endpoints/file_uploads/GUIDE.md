---
slug: file_uploads
role: capability
title: File uploads
summary: Accept user-uploaded files (images, PDFs, docs; audio needs allowed_types) - the multipart upload endpoint, the returned URL you render, optional table auto-insert, and the size/type caps.
use_when: Users upload or attach files, images, audio, or documents - avatars, photo posts, PDF attachments.
not_when: A project whose only images are author-supplied assets shipped with the site.
evidence_gated: true
category: endpoints
position: 26
requires: [api_response_contract]
tools: [manage_uploads]
---

# File uploads (any project with an upload endpoint)

`manage_uploads` wires a multipart upload endpoint; the platform stores the file under `/files` and hands you back its URL. You never write a server handler or touch the disk.

{{contract:endpoint-routes}}

## Post the file, then use the returned `url`

The body is `multipart/form-data` with the file under the field name **`file`** - not JSON, not a raw body, not a differently-named field. Build it with `FormData` and let the browser set the boundary (do NOT set `Content-Type` yourself):

```js
const fd = new FormData();
fd.append('file', input.files[0]);              // field MUST be "file"
const res = await fetch('/api/avatars/upload', { method: 'POST', body: fd });
const { url } = await res.json();
img.src = url;                                   // served from /files - render it directly
```

The response is a **bare object** `{ url, filename, size, type }` (not the LIST envelope - see `api_response_contract`). The durable thing to keep is `url`; store it on whatever row references the file (a `posts.image_url`, a `users.avatar`) via that row's normal `create_api` PUT/POST.

## The row carries user-typed fields too? Don't pass `table_name`

This is the common shape: the user picks a file **and** types something about it - a track title and DJ name, a photo caption, a document description. The upload cannot carry those. It fills exactly four columns (`filename`, `url`, `content_type`, `size`) and knows nothing about your form's other inputs.

So don't bind the table. Upload disk-only, then POST the row yourself with the url and the typed fields **in one insert**:

```js
const fd = new FormData();
fd.append('file', fileInput.files[0]);
const { url } = await (await fetch('/api/dj-tracks/upload', { method: 'POST', body: fd })).json();

await fetch('/api/dj-tracks', {                   // the table's normal create_api POST
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ url, dj_name: djInput.value, track_title: titleInput.value }),
});
```

The columns stay `NOT NULL` - the single insert supplies every one of them. No nullable-then-patch two-step, no server function. Reach for `table_name` only when the row is *nothing but* file metadata.

## Caps and gating are endpoint config, not client code

- **Size:** 5 MB default, raise with `max_size_mb` (hard cap 500). The client cannot override it; a too-big file is rejected server-side.
- **Type:** restrict with `allowed_types` MIME globs (e.g. `["image/*","application/pdf"]`); the default allow-list covers images, PDF, text, JSON, and zip. **Audio and video are NOT in the default**: set `allowed_types:["audio/*"]` (or `video/*`) or they are refused with 400. The check uses the SNIFFED type, and CSV/JSON/plain text sniff as `text/plain`, so use a glob (`text/*`), never an exact `text/csv`.
- **Auth:** set `requires_auth` for logged-in-only uploads - the POST then needs the bearer token, so send it through `apiFetch` (imported from `'auth'`, which carries the token) rather than a bare `fetch`. An upload feeding an owner-owned table follows the same ownership rules as any endpoint (see `api_response_contract`); scoping is not automatic - see "Ownership, folders and quotas" below.

<!-- extended -->

## Persisting the file's own row: the table needs the four metadata columns
> when: passing table_name so each upload auto-inserts its own row.

Pass `table_name` and each successful upload auto-inserts a row - but the table MUST already exist and have `filename`, `url`, `content_type`, and `size` columns. If any of those four is missing, **every upload fails** (500) and the file is deleted, so create the table with exactly those columns first.

The upload fills ONLY those four columns. If the linked table has any OTHER `NOT NULL` column with no default - a share `code`, an `expires_at` - the auto-insert can't supply it, so `manage_uploads` **rejects the binding** and no endpoint is created.

**The fix is a generated column, not a server function.** Declare those columns with `generate` in `manage_schema` create_table, so the DB fills them on every insert:

```
files: { code: {type:TEXT, unique:true, not_null:true, generate:"code"},
         expires_at: {type:TEXT, generate:"expires_in_days:7"},
         filename:TEXT, url:TEXT, content_type:TEXT, size:INTEGER }
```

Now the plain upload auto-insert into `files` just works - a short `code` and a 7-day `expires_at` land on every uploaded row with **no function, no detach, no trigger**. `generate` kinds: `code` (short slug), `token`, `uuid`, `expires_in_days:N`, `expires_in_hours:N` (create_table only). Fall back to disk-only (`table_name:"none"`) + a `manage_function` that `ctx.insert`s the row ONLY when the value needs real computation a generated column can't express. (A `before`/`transform` trigger can't do this - triggers fire AFTER a write commits, and the upload path raises no event.)

**Get the column set right at `create_table`.** `alter_table` only adds and drops columns - it cannot remove `NOT NULL` from an existing one, and it cannot attach `generate` (SQLite forbids a non-constant DEFAULT on ADD COLUMN). There is no "loosen it later". If a table is already wrong and still empty, drop and recreate it - dropping an empty table needs no owner approval.


## Ownership, folders and quotas
> when: uploads must be per-user, or you hit a 507, a rate limit, or a tenant path.

There is no automatic ownership scoping on an upload endpoint. Pass `owner_column` (the linked table's user-id column; needs `table_name` and `requires_auth=true`; `"none"` clears it on update) so the runtime stamps the uploader; a linked table whose `user_id` is `NOT NULL` is rejected as unfillable without it. Other settings: an optional `folder` form field, `rate_limit` (default 60), a 2 GiB per-project quota (507 when full), and a `tenant_column` forces a per-tenant path prefix.

## Showing upload progress
> when: the UI needs a progress bar for large files.

`fetch` cannot report upload progress. For a progress bar use `XMLHttpRequest` and its `upload.onprogress` event - the only browser API that surfaces bytes-sent - posting the same `FormData` with the `file` field to the same `/api/<path>/upload` route. For most image/avatar uploads a plain `fetch` with a spinner is enough; reach for XHR only when files are large enough that progress matters.

## Client-side validation is a courtesy, not the guard
> when: tempted to enforce size/type in JS.

Check size/type in JS only to give a fast, friendly error before the round-trip. It is not the real limit - `max_size_mb` and `allowed_types` on the endpoint are, and they hold even if a caller skips your page. Never treat a client-side check as the enforcement.
