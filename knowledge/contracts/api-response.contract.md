---
id: api-response
title: REST list/single response envelope
kind: json-literal
governs: []
probes: [offset, bare row]
---
**REST response shapes (any `create_api` endpoint).** The CRUD endpoints are asymmetric - get this wrong and a page throws `x.map is not a function` (list) or reads `undefined` (single row).

- **LIST** `GET /api/<path>` returns an ENVELOPE object, never a bare array:

```json
{ "rows": [ /* rows */ ], "count": 50, "limit": 50, "offset": 0 }
```

  Destructure `rows` first, then use array methods on it. The list array is `rows` - there is no `.data` field on the envelope. If you wrap `apiFetch` in your own helper, return the parsed body and read `.rows` off it - a `{ resp, data }` wrapper whose `data` you destructure again is the double-`.data` trap that throws `x.map is not a function`.
- **SINGLE ROW** `GET /api/<path>/<id>`, **POST**, and **PUT** return the BARE row object directly (no envelope wrapper).
- **DELETE** returns HTTP 200 with `{ "success": true }`.
- `count` is the size of THIS page (bounded by `limit`/`offset`), not the table's total row count.
- **Errors** (any non-2xx) are `{ "error": "<message>" }`: show `data.error`, never `data.message`.

**HTTP status codes (what `test_endpoint` should expect).** A successful **POST (create) returns 201 Created**, not 200 - asserting `expected_status: 200` on a create is a false failure. GET (list/single) and PUT return **200**; DELETE returns **200**; validation/auth failures return **4xx**, server errors **5xx**. `test_endpoint` accepts any 2xx by default, so leave `expected_status` unset unless you mean to pin a specific code.
