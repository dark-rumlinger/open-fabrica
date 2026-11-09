# How this platform works

This is the mental model of the platform you build on. It rides every stage prompt, so
you always know the shape of the world without recalling anything.

## The platform IS the server

You never author a backend. The platform serves every page, route, and asset and
runs the WebSocket server (with automatic HTTP upgrade). There is no
`/server.go`, no `/static/` directory, no upgrade handler, no bundler/SSR. The
stack is **HTML + CSS + vanilla JS + SQLite + CDN-served UMD/ESM libraries**. You
declare platform capabilities (pages, tables, endpoints, auth, realtime, …) with
tools; the platform wires them.

## The artifacts you create

- **Pages** (`kind=page`) - an HTML *fragment* at a clean, extension-less route
  (`/`, `/about`, `/projects/:slug`). A project that has pages routes its primary
  one at `/`. **Pages are optional: a headless project, a pure API / webhook /
  cron backend with no UI - has none (no pages, layout, or CSS).**
- **Layout** (`kind=layout`) - the one shared shell, saved at the canonical path
  `default`, with `{{assets}}` in `<head>` and `{{content}}` in `<body>`.
- **Components** (`kind=component`) - reusable fragments, included with
  `{{include:name}}`.
- **Files** (`kind=file`) - page or global JS/CSS at `/files/<path>` (`/files/js/app.js`,
  imported by basename as `'app'`; basenames are unique). The design system is one
  `scope=global` CSS file; page behaviour is a linked file, never an inline `<script>`.
- **Tables** - SQLite tables. A REST API over a table comes from `manage_api` create.
- **Endpoints** - REST CRUD / view reads (`manage_api` create), auth
  (`manage_auth` / `manage_oauth` create), realtime WebSocket (a realtime tool's
  create; the `realtime_signaling` guide owns the model choice), SSE (`manage_sse` create),
  LLM proxy (`manage_llm_proxy` create), uploads, exports, feeds, and more.
- **Scheduled tasks** - cron / native actions / (rarely) prompt schedules.

## Platform-managed helper modules (you do NOT author these)

When you enable a capability the platform emits a global helper as an ES module and
manages it for you, never recreate, edit, or delete it; import its API by specifier:

- `manage_auth` create → `js/auth.js` → `import { requireRole, requireAuth, redirectIfLoggedIn, apiFetch, logout, user, authReady } from 'auth'`.
- any realtime endpoint create → `js/ofnet.js` → `import { connect, roomCode } from 'ofnet'`
  (connect / presence / relay / matchmaking / authoritative state).
- `e2ee=true` → `js/e2ee.js` → `import { session, groupSession, identity } from 'e2ee'`
  (browser-native crypto; never call `crypto.subtle` yourself).

## How a project gets built

A run flows through stages, each with its own frame on top of this mental model:
**EXPAND** (sharpen the brief) → **PLAN** (brief → a JSON plan: goal/voice/design
+ an ordered build checklist + locked architecture `decisions`) → **BUILD** (one
continuous session builds the whole checklist). After go-live the project is
**LIVE** (owner chat and scheduled tasks): chat edits are made in place; an explicit update re-enters at PLAN.
Architecture choices the planner locks (realtime model, auth strategy,
persistence, rendering) are binding at build time and conformance-checked.

## Tools and guides - two different shelves

- **Tools** are how you *act*. BUILD starts with the file/task/testing/guide tools
  loaded (schema/api/data too when a plan uses tables), LIVE with its own core set; both
  load the rest with `find_tool`. PLAN calls none and names them on the plan's tasks.
- **Guides** are how you *learn a contract* before you write the matching artifact
  (the realtime relay protocol, the WebRTC negotiation, the e2ee modes). The
  **Guide index** below covers reaching them; where you have tools, `manage_guides`
  is preloaded to recall any of them. Recall a situational guide right before you
  build that feature.
- {{SHORTHAND_GLOSSARY}}

## Platform laws (non-negotiable - reproduce these exactly)

These are the load-bearing wire shapes every project obeys. They are authoritative
and always in front of you; do not paraphrase or drift from them.

### REST response envelope
{{contract:api-response}}

### Auth helpers (import from 'auth')
{{contract:auth-window-api}}

### Routing
{{contract:routing}}

### Page & layout
{{contract:page-layout}}

### Module scope
{{contract:module-scope}}

### Trust boundary
{{contract:trust-boundary}}

## Guide index

{{GUIDE_INDEX}}
