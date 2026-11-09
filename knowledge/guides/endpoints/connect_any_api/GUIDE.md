---
slug: connect_any_api
role: capability
title: Connect any external API
summary: Talk to any third-party HTTP API (a SaaS, a data source, a LAN device like Home Assistant) - store the credential, register the provider once, call it by name, and pick the build-time vs runtime path.
category: endpoints
position: 29
tools: [manage_providers]
---

# Connect any external API

The provider registry is how a project talks to an outside HTTP API without
scattering base URLs and keys through calls. The workflow is always three
steps, in this order:

1. **Store the credential**: `manage_chat ask` (`question_type: "secret"`,
   `secret_name: "ACME_KEY"`) for the owner's key, `manage_secrets` `store` only
   for a value you generated. The raw key never appears in a tool argument again.
2. **Register the provider once**: `manage_providers` `add` with `name`,
   `base_url`, the auth shape, and `secret_name` pointing at the stored secret.
3. **Call it by name**: `manage_providers` `request` with `provider`, `method`,
   `path` (appended to the base URL), and an optional JSON `request_body`. The
   platform injects the credential in the registered shape automatically.
   `method` defaults to **POST** (set `GET` for reads).

**Auth shapes** (`auth_type` on `add`): `bearer` (default -
`Authorization: Bearer <secret>`), `api_key_header` (custom header - set
`auth_header`, e.g. `X-Api-Key`; no prefix, `auth_prefix` is `bearer` only), `basic`
(store the secret as `user:pass`; the platform base64-encodes it), `none`
(keyless API).

## Choose: one-shot, registered provider, or runtime call

- **One-shot build-time probe** of a public URL - `manage_http` `request`.
  Registering a provider for a single call is overhead.
- **The project hits the same API repeatedly, or the API needs a key** -
  register a provider and use `request`. Credentials and auth shape live in
  one place; every later call is just `provider` + `path`.
- **The RUNNING APP must call the API** (a page action, a user click) -
  that is a server function (`manage_function`) reading the key with
  `ctx.secret` and calling `ctx.fetch`. `manage_providers` `request` is a
  build/agent-time verb: it is NOT reachable from the app's pages, and the
  key must never be sent to the browser.

<!-- extended -->

## Discovering an unfamiliar API
> when: you have a docs or OpenAPI/Swagger URL and don't know the operations.

`manage_providers` `describe` fetches the provider's OpenAPI/Swagger JSON
(`docs_url`, or the stored `api_docs` when that is a URL) and returns a
compact list of callable operations (method, path, summary). Pass `filter`
to keep only operations whose path or summary matches a substring. Do this
before guessing paths against a real API.

## LAN devices (Home Assistant and friends)
> when: the brief wants to reach a device on the operator's own network.

Outbound calls are SSRF-guarded: private, link-local, and metadata addresses
are blocked, on the first request and on every redirect. A trusted device on
the operator's own network is the one exception: register it with
`allow_private_host: true` and the block lifts for that provider's own
`base_url` host ONLY - every other internal address stays blocked.

Home Assistant example: store the long-lived token as secret `HA_TOKEN`, then
`add` name `home_assistant`, `base_url` `http://homeassistant.local:8123/api`,
`auth_type` `bearer`, `secret_name` `HA_TOKEN`, `allow_private_host` true.
Turn on a light with `request`: method `POST`, path
`/services/light/turn_on`, request_body `{"entity_id":"light.living_room"}`.

## Housekeeping
> when: a key rotates, a base URL moves, or a provider is retired.

A `request` response body is capped at 8000 characters and carries `body_truncated` when cut.
`update` patches only the fields you pass (description, `api_docs`,
`base_url`, `auth_type`, `secret_name`). It cannot change `auth_header`,
`auth_prefix` or `allow_private_host`: to change those, `remove` the provider and `add`
it again. `list` shows every registered
provider with its auth shape and secret name (never the secret value);
`remove` deletes by name or id. Rotating a credential is a `manage_secrets`
`store` under the same name - the provider record does not change.
