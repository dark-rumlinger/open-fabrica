---
id: endpoint-routes
title: Non-CRUD endpoint route shapes
kind: kv
governs: [file_uploads, server_push, seo_and_syndication]
probes: [/upload, /stream, /feed/]
---
**Non-CRUD endpoint routes.** The endpoint families below do NOT live at the bare `/api/<path>` like CRUD does - each has its OWN route suffix and consumption shape. Reproduce these exactly: a page that POSTs an upload to `/api/<path>`, or reads an SSE stream with `fetch().json()`, fails silently and no gate catches it. CRUD (`create_api`) keeps the `/api/<path>` shapes in the `api-response` contract; this contract covers only the families here.

| family | tool | route | client / shape |
|---|---|---|---|
| File upload | `manage_uploads` | `POST /api/<path>/upload` | multipart form, field name **`file`**; `201` -> `{ url, filename, size, type }`, plus `row` (the inserted row, generated columns too) with a `table_name`. Render the returned `url` (the file is served from `/files`). |
| Server push (SSE) | `manage_sse` | `GET /api/<path>/stream` | `new EventSource('/api/<path>/stream')` then `source.addEventListener('data.insert'\|'data.update'\|'data.delete', e => JSON.parse(e.data))`. One-way server->client; never `fetch().json()` it. On a `requires_auth` endpoint append `?token=<auth_token>` - EventSource cannot send an Authorization header. |
| CSV export | `manage_export_endpoint` | `GET /api/<path>/export` | navigate / link to download `text/csv`. |
| CSV import | `manage_import_endpoint` | `POST /api/<path>/import` | multipart form, field name **`file`**, carrying the CSV. |
| RSS / Atom feed | `manage_feeds` | `GET /feed/<path>` | an XML feed, **not** under `/api`. Advertise it from the layout `<head>`: `<link rel="alternate" type="application/rss+xml" href="/feed/<path>">`. |

The path suffix is fixed by the platform - you do not choose it. `<path>` is the endpoint's own suffix (no `/api/` prefix in the tool arg); the suffix and the `/feed/` prefix are added by the runtime.
