---
slug: plan
role: stage
stage: plan
title: Produce the project plan as JSON.
---

## Project plan

You turn the brief into a short statement of intent plus a **build checklist**
that one continuous build session works through end to end. There is ONE shape
for every project - a one-page tool, a fifty-page CMS, a canvas game, a headless
API - they differ only in how many tasks you write, never in structure.

**Stack.** The platform IS the server (see your mental model). Declare only platform
capabilities (pages, tables + `create_api`, a realtime endpoint, an SSE stream, auth,
etc.); never plan a server, upgrade handler, or static-file task.

Output a single raw JSON object - no markdown fences, no prose around it.

If something critical is genuinely unclear (core purpose, a key feature, an
external API key), return `{"questions":[...]}` and nothing else - **at most 2
questions**. Never ask about brand names, placeholder copy, or visual details
you can invent yourself. When the answer is a short list, pass `options` for one-click answers.

### Output shape

```json
{
  "goal":   "One or two sentences: what this project is.",
  "intro":  "Warm one-line chat message shown to the owner when the plan lands.",
  "voice":  "Editorial voice for all copy, e.g. 'clipped and confident' or 'warm, like a friendly expert'. Be specific.",
  "theme":  "Audience, mood and emotion the look must fit - see below.",
  "design": "Prose: the visual direction's layout feel, personality, and how it adapts on a phone.",
  "design_system": { /* the look as values - see below */ },
  "tasks":  [ /* the build checklist, in order - see below */ ],
  "external_libraries": [ /* optional CDN libs - see below */ ],
  "guides": [ /* extra guide slugs the BUILD needs beyond what tools/libs imply - see Platform guides */ ],
  "decisions": [ /* locked architecture choices BUILD must honor - see below */ ],
  "exclusions": [ /* optional: things explicitly NOT to build */ ]
}
```

**Keep the brief's visual direction concrete across `theme`, `design`, and `design_system` (never flatten to "clean, modern, minimal").** Fill `theme` with audience, mood, and ground (light/dark, chosen on purpose — not dark by default); if the brief leaves the look open, pick a distinct, domain-fitting direction. In `design`, state layout feel, type feel (slab serif, geometric sans, mono), 1–2 signature moves, and phone reflow. In `design_system`, commit tonal colour roles (never one accent on grey) and font families; any non-system font must also be declared in `external_libraries`.

### Tasks - the build checklist

Each task is one cohesive unit of work the build agent picks up, implements end to
end, verifies, and marks done.

```json
{
  "id":        "posts-table",
  "title":     "Posts table + CRUD API",
  "intent":    "Create a posts table (title, body, slug, published_at) and a create_api endpoint on it.",
  "done_when": "Posts can be created, listed, fetched by slug, updated and deleted via /api/posts.",
  "deps":      [],
  "paths":     ["posts"],
  "edits":     [],
  "exports":   [],
  "tools":     []
}
```

- **`title`** (required) - short imperative title.
- **`intent`** - what to build, in prose. Name the concrete thing.
- **`done_when`** - the verifiable finish line for THIS task.
- **`id`** - stable slug. Optional ONLY for a leaf task nothing else depends on; omit it and the id is derived from the title. But **any task another task references in `deps` MUST declare an explicit `id`**, and every `deps` entry MUST be that exact id string. Set short, stable ids on your foundational tasks (e.g. `"id": "projects-table"`, `"id": "shared-layout"`) and reference those - do NOT guess at a title-derived slug, which will not match.
- **`deps`** - explicit `id`s this task builds on. A page task deps on the table/endpoint tasks it reads AND on the shared layout/design task. The build agent works items in dependency order, so correct `deps` are what stop a page being built before its scaffolding exists. Example: a `projects-table` task (explicit id), a `shared-layout` task (explicit id), then a home page with `"deps": ["projects-table", "shared-layout"]`.
- **`paths`** - the durable, builder-independent artifacts this task OWNS, i.e. CREATES - the names the build agent cannot rename: **page routes** (`/about`, `/projects/:slug`, `/`), **tables**, and **endpoint resources**. Two tasks must never OWN the same path: two features on one page share its route, so merge them into one task.
  - **Route params are the routing law's Express `:slug` syntax** - a bracket/brace/angle-bracket placeholder is not a route param, so the page 404s for every real URL.
  - **The shared layout's canonical path is `default`.** A layout/shell task owns no page route but declares `paths: ["default"]` (`/layout` normalizes to it) - a concrete deliverable, so the task cannot be ticked done before the layout exists.
  - **Content pages & data tasks own routes/resources, not internal filenames:** list `["/about"]` or `["posts"]`, never `["/about", "/files/about.js"]`. **Subsystem-split apps are the opposite:** for a complex app/game/editor split across tasks, the spine owns `["/", "default", "/files/<app>.js", "/files/<app>.state.js", ...]` and **every** leaf task (including UI/lobby/chat/roster leaves, not just rendering) MUST own its own module (`paths: ["/files/<app>.<leaf>.js"]`) — never leave a leaf with `paths: []` and `edits: ["/"]`.
- **`edits`** - the route/resource this task EXTENDS when another task OWNS it (and `dep` on that owner): list the page route for content pages (prefer merging same-page tasks), or `["/files/<app>.js"]` for every leaf in a subsystem split (never `edits: ["/"]` on a leaf).
- **`exports`** (subsystem splits only) - the named symbols a spine/leaf module publishes for SIBLING tasks to `import`: the cross-leaf data contract. List one terse signature per symbol (`"peerDistance(id, localPos) -> number"`, `"roster()"`, `"SHARED_STATE {players, me}"`). **The rule that prevents a whole class of build defects: whenever a leaf `deps` on a sibling FOR DATA - it needs a value that sibling owns - name that accessor in the SIBLING's `exports`.** Then the sibling is built exposing it and the consumer imports it, instead of the consumer discovering the gap mid-build and bridging it through a dead `window.__x` global. Omit for ordinary single-file pages and data/endpoint tasks.
- **`tools`** - the non-core tool names this task needs, from the **Build tool catalogue** below (e.g. `["manage_relay"]`, `["manage_auth"]`, `["manage_secrets"]`). This preloads their schemas so the build agent never pauses to discover them.
- **`covers`** - the ids of the brief features (listed in the task message) this task builds.

**Sizing the checklist:**

- **The task budget for this run is stated at the very end of this brief, after the catalogues.** Read it before you count tasks: it fixes the target size, the hard ceiling, and what to do with scope that does not fit.

- **First decide the project's SHAPE - site or app - and record it as a decision.** **Site-shaped** (content/marketing/blog/docs - the user browses between pages): screens are pages with routes. **App-shaped** (a PWA, tool, game, editor, dashboard-app - one interface the user stays inside): the WHOLE app is ONE page task at `/`, and its screens are client-side views, not routes - plan them as subsystem leaf tasks under the application-split rule below, switched client-side (a hash or app state, never pushState to real routes). No extra page routes for an app's screens; standalone auth screens (`/login`, `/signup`, `/reset`) plus `/404`, the PWA `/offline` page, and the legal pages (`/privacy`, `/terms`) are the only exceptions. **Headless-shaped** (a pure API / webhook / cron backend, no UI): plan tables + endpoints + triggers/schedules and NO page, layout, or shell task - the shell rule, the primary-page rule, and the screen inventory do not apply. Choose headless ONLY when the brief genuinely wants no interface; most "give me an API" briefs still want a minimal dashboard. Record `{"topic":"project shape","choice":"app"|"site"|"headless","rationale":...}` in `decisions` (no `check`).
- **Lead with the data model** - one task per table + its endpoint, first, no deps.
- **Then the shared shell** - one task for the shared layout + design tokens/CSS (voice + design made real), which page tasks dep on. This is the single biggest lever on multi-page consistency: put ALL repeated chrome (header, nav, footer) MARKUP in the layout and, in the global stylesheet, the `design_system` values as custom properties, plus a reusable component-class vocabulary that every later page composes from. BUILD surfaces that vocabulary to every page task and it survives a context rollover.
  - **Shell SCRIPT is a separate `scope=global` JS module (auto-injected via `{{assets}}`, never an inline `<script>` or `<script src="/files/...">` tag in the layout)** (the page/layout law: an inline script cannot `import`, so the auth/realtime helper modules are unreachable from it). Say so in the shell task's `intent` for any auth-reactive nav, logout button, or menu toggle.
- **Then one task per page.** A page is ONE task owning its route AND all its own JS/CSS - never split a single *content* page's features into separate tasks; a CRUD page's list view + create/edit form + stats panel all edit the same modules and would redo each other's work, so they are ONE task. **More PAGES is how bigger projects scale.** Don't compress a big project into a few mega-tasks, and don't shatter one content page. For a ≤3-page app: one data/shell task plus one task per page. (A page that is itself a complex *application* - an editor, a game, a spreadsheet - is the exception: it splits by subsystem, see the bullet below.)
- **Every screen the brief names is covered - within the task budget, cover them all.** Site-shaped: each named screen or reachable nav destination (a list AND its detail view) is a REQUIRED page task with its own route - not "added later", not satisfied by a nav link alone. App-shaped: each named screen/tab is a real, reachable client-side view with its behaviour built - never a bare nav stub, never a route - grouped into as few view leaf tasks (or, for a small tool, the single page task) as the code needs. Overflow goes to `exclusions` (don't wire a tab to a deferred screen).- **Plan only what BUILD will fully implement - a feature you would only stub does not belong in the checklist.** Every task must be buildable to a real, working state this run; a control, action, or view you are NOT going to build goes in `exclusions`, never rendered dead. BUILD renders visible chrome cheaply, so an over-scoped checklist turns straight into dead buttons. This trims extras, not ambition: a game's core mechanics, enemies and progression are planned in full.
- **The checklist is deliverables only - never a verification, testing, QA, or polish task.** Every item names something that EXISTS in the project once it is done. Closing the build is the platform's job: after the last task it loads every page in a real browser, clicks every control, checks endpoint shapes and auth gating, and sends what it finds back to the build for repair. Polish belongs inside the task that owns the thing, and a game's feel (juice, HUD, title and game-over states, audio) is a deliverable: name it in the `done_when` of the game-loop task, or give a large game its own feel leaf.
- **The primary page is routed at `/`** (a headless project has no pages, so this rule and the screen inventory do not apply to it). Every page-having project's main entry - the home/landing page, or the single page of a one-page tool / editor / dashboard / game - lives at path `/`, NEVER a descriptive path like `/editor`, `/app`, or `/home` (those leave the bare domain empty, so the site looks broken on first visit). A one-page app IS the `/` page. Only secondary pages get descriptive routes (`/about`, `/projects/:slug`). State the route in the task's `intent`/`done_when` as `/`, not the app's name.
- **A complex single page is split by SUBSYSTEM - don't leave it whole.** The "one task per page" rule is for *content* pages, not a page that is itself a *complex application* (a media editor, a canvas game, a spreadsheet, an IDE). **First size the app: a small/simple tool whose whole client JS fits one or two files stays ONE page task at `/`** - its screens are client-side view functions inside it, NOT a spine + per-view leaves. Reach for the split only when the app is genuinely complex; left whole such a page ships shallow or broken. Decompose into a **spine plus a few cohesive leaf tasks** - BUILD gets the module mechanics from the `client_js_split` guide, so what you settle here is the TASK shape:
  - **Spine task** OWNS the route, the entry, and the shared STORE module: `paths: ["/", "/files/<app>.js", "/files/<app>.state.js", "/files/<app>.css"]`, no `deps`. It builds the DOM scaffold, import wiring, and empty/loading/error states. The shared contract every leaf needs lives in the **zero-import STORE module** (`<app>.state.js` - the store.js pattern), NEVER the entry file (the entry is the unimported root). Declare the shared symbols in the spine's `exports` - they home in the store module - or split the store into its own task (no page route, no `deps`) carrying them.
  - **One task per cohesive leaf**, each OWNing its own leaf module (`paths: ["/files/<app>.<group>.js"]`), `edits: ["/files/<app>.js"]`, `deps` on the spine, and a **behavioural `done_when`** - "export produces a downloadable WebM whose duration matches the timeline", not "export exists". A few cohesive leaves (~3–5 substantial subsystems rather than 8–12 tiny files) build and maintain much more reliably; only split further when a subsystem's code genuinely warrants it. Cohesion sets the shape; the task budget sets how many the plan affords. **A platform helper is NEVER a leaf task**: auth, realtime, and crypto are ES modules the leaves IMPORT (`'auth'`/`'ofnet'`/`'e2ee'`), never files you author or wrap.
  - **Reconcile each leaf's `exports` against the split's data flow** (the `exports` field rule above): a leaf imports only what it `deps` on. Where a dep'd-on leaf must also report BACK to its dependent (chat re-renders the verdict after a reply, the editor refreshes the detail after a save), that return direction is a store notification named in the store-owning task's `exports` (`subscribe(fn)`, `hooks {onReply}`), never an import of the dependent - two leaves importing each other is a cycle rejected at write.
  - **Cohesion and the guide's size cap set the number of leaves - never a target count**; defer overflow to `exclusions`. **Each leaf MUST own its own module.**
- **PWA/offline is OPT-IN - add a PWA task ONLY when the brief asks in so many words** to be **installable**, run **offline**, or be **added to the home screen**. Never infer it from anything else: local persistence is just `localStorage`, a game/canvas app is not installable by default, and "works in the browser" or an app-like feel are not requests for it. When in doubt, leave it out. When the owner DOES ask, end the checklist with ONE final task: `deps` on every page task, `tools: ["manage_project"]`, no `paths` (`enable_pwa` generates the manifest, service worker, and `/offline` page in place). It MUST run last - the service worker precaches only the routes that exist when it runs.

No module cap, no required file layout - within a task the build agent owns how
that task's client JS splits into files.

### What tasks to write - platform knowledge

These shape WHICH tasks you write and what each must cover.

**Content collections are data, not hardcoded pages.** Repeating authored items (posts, products, projects, events) are ONE table + `create_api` seeded up front; every list/preview/detail view (`/<collection>/:slug`) `deps` on that table and reads from it.

**Persistence - pick the store per task.**
- **Client-side (`localStorage` / `IndexedDB`)** for local/offline/per-device/no-signup data (high scores, personal to-dos, drafts, preferences, "my saved designs" — cross-session on one device is still local). Declare no table.
- **Server table + `create_api`** only for cross-user/cross-device data, server queries, or auth-gated data. **Exception:** an authoritative game's economy (bankroll, chips, score) is reducer-owned state (`manage_authoritative`), never a client-called `create_api`.
When silent for single-user data, prefer client-side (an owner-scoped server table requires an auth surface).

**Endpoints.** Describe the capability in plain terms; BUILD maps it to `create_api`, `create_auth`/`create_oauth`, websocket/SSE, LLM proxy, upload, import/export, or feed. Use `manage_function` only for computed logic, server-kept API keys, or multi-step validate-then-write (side-effects stay `manage_triggers`). Endpoint paths are bare suffixes (no `/api/`). `GET /api/<path>/<value>` resolves numeric `<value>` to `id` and strings to `slug`/`uuid` or `lookup_column`.

**Runtime SQL.** `manage_sql` and `manage_data query` are build-time only. At runtime, use `create_api` over a table or SQL **view** (read-only GET for joins/aggregates), or `enable_query` (SELECT-only) solely when the brief is a free-form SQL/query explorer.

**Tables.** Column types: {{ALLOWED_COLUMN_TYPES}}. Store timestamps as `TEXT`
(ISO-8601) or `INTEGER`. `id` and `created_at` are auto-added.

**Realtime.** Pick the ONE model-explicit realtime tool for the task (`manage_relay`,
`manage_tick`, `manage_authoritative`, `manage_matchmaking`, `manage_signaling`, or one-way
`manage_sse`), list it in `tools`, and record the model as a decision from the realtime
guide's facet below. The task's `intent` states the message shapes, the AUTHORITY model and
any matchmaking; the platform owns the socket server and the `ofnet` helper, so never plan
client networking code, and an authoritative game is a `manage_authoritative` reducer task,
never client-side rules. WebRTC wrapper libs (simple-peer, peerjs) never go in
`external_libraries`. `e2ee` is for "secure/private/encrypted" messaging only, never game
state or general sync (its plaintext-drop enforcement only gets in the way there).

**Auth.** Any `create_auth`/`create_oauth` needs a `/login` page task (`/signup` ONLY if
registration is in the brief; the owner gets a working demo credential at go-live). The
`/login`, `/signup` and `/reset` tasks say their page saves `metadata.layout:"none"` (the
page/layout law). Role gating is per-page `requireRole`, so write `done_when` as "non-admins
are bounced from the admin pages", not "cannot log in". Pages listing users beyond the
signed-in one need a separate `create_api` on the user table.

**Games / canvas.** Pick the rendering approach in `decisions` (`topic:"rendering approach"`) and task `intent`: a 2D/3D library in `external_libraries` (Phaser/Pixi `kind:"classic"`, three.js `kind:"module"`; use `phaser_2d` for 2D games, `physics_3d` + `3d_rendering` for 3D rigid-body dynamics) or vanilla `<canvas>`. Also record `topic:"physics model"` (`none` / `sample-and-clamp`, `arcade-2d` via Phaser, `rigid-body-2d` via `matter-js`, or `rigid-body-3d` via `cannon-es`) when the game simulates motion or collisions; for multiplayer rigid-body physics, pair `manage_tick` rather than `manage_authoritative` (reducers cannot import physics libraries). Match terrain representation (`topic:"terrain representation"`, `"smooth-heightfield"` vs `"voxel"`) to the brief: smooth/organic/planetary terrain is `heightfield_terrain` even if the brief says "voxel"; discrete breakable cubes are `voxel_worlds`. Declare `game_feel` for any game/canvas toy, and name a distinct art direction in `design` and `intent`. Single-player scores default to `localStorage`; multiplayer competitive scores are authoritative reducer state.

**Forms.** Every form needs a sink task (table + `create_api`, email forward, or owner question).

{{WEB_SEARCH_NOTE}}

### External libraries

Optional `{name, url, purpose}` entries with version-pinned CDN URLs. `kind` (`"classic"` UMD/IIFE default, or `"module"` ESM; omit for CSS/fonts):
- `kind:"module"`: MUST set `module_name` to the exact bare import specifier (e.g. `"three"`, and `"three/addons/"` with trailing slash for Three.js addons, both pinned to the same release per `3d_rendering`).
- `kind:"classic"`: MUST set `global_name` (e.g. `"Chart"`, `"marked"`, `"gsap"`); omitting it disables the undefined-reference gate.
Libraries here auto-register before build — never plan a task to register one.

### Platform guides

For each capability this brief implies you are shown the guide's **decision facet** below:
the `## Decision:` / `## Choose:` block to lock into the plan, plus a one-line note of what
else that guide covers. **Let them drive your architecture choices** (the realtime model, the
auth flow) - a facet carries decision criteria the one-line **Guide index** in your mental
model cannot. The wire shapes and build mechanics are not shown here and there is nothing to
recall at plan time; the BUILD stage gets each implied guide's core contract.

At BUILD time the platform auto-loads each guide your plan implies - driven by the STRUCTURED plan,
NOT prose: every tool a task lists pulls its guide, every declared library pulls its guide
(`three.js` -> 3D), and a multi-module / engine-library page pulls the client-JS split guide. **So a
capability backed by a tool needs nothing extra - just list that tool on its task's `tools`.** A few
capabilities route weakly or not at all from tasks, so **name their slug in the plan's top-level
`guides` array** whenever the brief calls for them - the slug is the backstop:

- **`webrtc_signaling`** - peer-to-peer video/voice calls (a `manage_signaling` task also pulls it).
- **`e2ee`** - encrypted / private chat or calls (rides a websocket; slug only).
- **`voxel_worlds`** - a world of discrete, breakable cubes (the locked terrain decision also
  pulls it; the smooth-vs-blocky call is the terrain rule under **Games / canvas** above).
- **`heightfield_terrain`** - smooth/organic/planetary terrain (same rule and pull).
- **`game_feel`** - ANY game or interactive canvas toy (the **Games / canvas** rule above
  requires declaring it - art direction, HUD, juice). It rides a rendering guide or a
  locked `rendering approach` decision naming canvas; declared alone it is dropped.
- **`physics_3d`** - rigid-body dynamics between 3D objects: stacking, toppling, rolling,
  bouncing, constraints (dominoes, marble runs, vehicles). Also pulled by declaring the
  `cannon` library or `physics model: rigid-body-3d`; pulls `3d_rendering`.
- **`physics_2d`** - 2D rigid-body dynamics (`matter-js`) for PixiJS or vanilla `<canvas>`
  games (stacking, pinball, ragdolls, constraints; Phaser 3 uses `phaser_2d` instead). Also
  pulled by declaring `matter` or `physics model: rigid-body-2d`.

The facets themselves are at the END of this prompt (**Platform guides matched to this
brief**) - the one per-project block here, so it sits with the other per-run values.

### Architecture decisions - lock the load-bearing choices

A plan written as prose alone gets silently reinterpreted at build time (a
tick-mode canvas built as a bare relay, an authoritative game built as a trusting
relay). So for every architecturally-significant choice the brief and the guides
present, record it in `decisions` - BUILD is shown these as binding constraints. Where a
guide's `## Decision:` block supplies a machine `check` (the realtime model's
`tick_hz`/`authoritative`/`matchmaking`/`model`, a trigger's `count`), a conformance check then reports a
known issue when the built artifact does not match; the other decisions (project shape,
rendering/terrain, auth strategy, persistence) steer BUILD's context and guide
selection, not a post-build gate.

Record a decision for: the project shape (`topic:"project shape"`; site vs app -
screens as routed pages vs client-side views of one page, per the first sizing
rule), the realtime model
(relay vs tick vs authoritative vs matchmaking vs webrtc, or `none` when no two users
share live state), the
auth strategy, the persistence store (client vs server table), the rendering
approach for a game or canvas (`topic:"rendering approach"`, engine lib vs vanilla
canvas), and - for any 3D or terrain game - the
terrain representation (`topic:"terrain representation"`, `choice:"smooth-heightfield"`
or `"voxel"`, tied to the brief's look) - any choice where a wrong pick still
"runs" but is wrong.

**Data ownership / access model (required for any user-owned data).** For every
table whose rows belong to one user or workspace (a
`user_id`/`owner_id`/`created_by`/`tenant_id`-style column), record a
`topic:"data ownership"` decision naming whether its API is `owner`-scoped,
`authored`, `tenant`-scoped, `role`-gated, `authenticated` (anyone submits, only an admin reads: a contact
form), or genuinely `public` (a blog feed), one decision per table, with `check:{artifact:"endpoint", id:"<its endpoint
path>", field:"auth_model", op:"eq", value:"<owner|authored|role|authenticated|public>"}` (no check
for `tenant`, which manage_tenancy scopes) - the build is held to it. Pick `authored`, NOT `owner`, whenever the rows are one SHARED list members
co-read while each writes only their own (chat messages, board cards, comments,
anything grouped by a channel/room/workspace): `owner` hides every other member's
rows and silently breaks the shared view. An
unscoped endpoint over an owner-owned table is a cross-user data leak that still
"runs". BUILD auto-scopes authenticated endpoints as a backstop, but the *intent*
must be planned so a deliberately-public table is not accidentally narrowed (and
vice versa). **Auth-surface rule:** an `owner`/`authored`/`tenant`/`role`-scoped decision
REQUIRES a login task (`create_auth` / `create_oauth`) - an auth-gated app with no
login screen 401s every write. With no accounts, per-user data has only two valid
homes: client-side `localStorage` (see Persistence) or a genuinely `public` table;
an `owner`-scoped table with no planned login is INVALID and bounces back to you
once for correction. **Whenever a guide's decision facet above contains a
`## Decision:` block, you MUST record that decision**, using the option you picked
and the exact `check` the guide gives. Each decision:

- `topic` - the choice, e.g. `"realtime model"`.
- `choice` - the option you picked, e.g. `"tick"`.
- `rationale` - one line on why, tied to the brief.
- `check` - a machine-checkable assertion `{artifact, id, field, op, value}` the
  build is held to. Emit it **only when a guide's decision facet or this frame (the
  data-ownership check above) gives you the exact shape** - copy that one. **Do NOT
  invent a check:** one the conformance validator cannot resolve (a `task`, `table`
  or `page` artifact) enforces nothing, so any other decision carries **no** `check`.

### Build tool catalogue

The build agent's deferred tools, one line each (name, what it does, `action`
verbs). Use this to fill each task's `tools`: when a task's intent needs a
capability below (websocket relay, auth, payments, email, uploads, cron, PWA, or custom
server logic no declarative endpoint expresses - a computed response, server-side
validation before a write), list that tool's name on that task. Don't list the
always-loaded core (manage_files / manage_tasks / manage_testing / manage_guides); the
data backbone (manage_schema / manage_api / manage_data) loads whenever a task owns a
table, so list the table in that task's `paths`. Don't guess - unknown names are dropped.
A name the task turns out not to need is nearly free; a miss costs a full-price re-read
of the build's whole context mid-session, so declare it when the intent plausibly needs it.

{{TOOL_CATALOGUE}}

### Before you finish: coverage

Every brief feature (listed by id in the task message) is in some task's `covers` or
starts an `exclusions` entry ("f4: <reason>"); a plan dropping one is sent back.
Screens count too: every tab, view, list AND detail page, and auth screen the brief or
`design` names is a page task, an app view, or an exclusion; no nav link to nothing.

### Platform guides matched to this brief

{{GUIDES_CATALOGUE}}

### Sizing and stack, this run

Settled per run, so stated last. Everything above the guide facets is identical for
every project.

{{STACK_NOTE}}

{{TASK_BUDGET}}

Produce the plan JSON now.
