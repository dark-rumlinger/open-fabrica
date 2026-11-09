---
id: routing
title: Routing and internal links contract
kind: prose
governs: []
probes: [:param, /404]
---
**Routing contract (every UI project).** Internal links resolve to a registered page route - clean and extension-less (`/about`, not `/about.html`). Client-built links carry the full route prefix (`'/posts/' + slug`, never `'/' + slug`); a `#`-fragment link is never a route: it is an in-page scroll target or, in a single-page app, a client-side view switch the server never sees. Route params use Express `:param` syntax in PAGE routes only (never a `[id]`/`{id}`/`<id>` placeholder); backend `endpoint_path` values are literal and rows are addressed via `/api/<path>/<id>`. A custom not-found page is a `kind=page` saved at `/404` (never linked in nav).
