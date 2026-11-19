---
slug: user_flows
role: capability
title: User flows (of_test.js)
summary: The of_test.js user-flow script (an experimental, opt-in owner setting) a project with pages or an API ships when the owner turned user-flow tests on - step verbs, users, email/API steps, what to cover. Recall it before writing of_test.js.
category: frontend
position: 34
---

# User flows (`of_test.js`)

When user-flow tests are enabled (`project_state` says when off), save `of_test.js` via `manage_testing action=write_flows` (replays immediately). Format: `export default { flows: [{ name, viewport?, users?, steps: [...] }] }` (plain data, no imports/functions, ≤5 flows, ~12 steps each). Recall `writing-flows` before writing/updating. Fix app bugs in code (never weaken assertions or security to pass a flow).

<!-- extended -->

## Writing flows
> when: you are about to write or re-save `of_test.js`.

- **Read before writing:** `get`/`outline` pages and scripts to copy selectors/labels verbatim.
- **Full-file replace:** start from `manage_files get of_test.js`; keep working flows (a save dropping existing flows is refused unless listed in `drop`).
- **Act:** `{goto:"/path"}`, `{reload:true}`, `{leave:true, as:"bob"}`, `{click:sel, at?:[x,y]}`, `{dblclick:sel}`, `{hover:sel}`, `{drag:sel, to:sel}` or `{drag:sel, path:[[x,y],...]}`, `{fill:sel, value}`, `{type:sel, value}`, `{select:sel, value}`, `{check:sel}`, `{uncheck:sel}`, `{press:"ArrowLeft"|"Enter"|"Space"|"Shift+Enter", hold?:ms}`, `{upload:sel, value:"image.png"|"photo.jpg"|"notes.txt"|"data.csv"|"data.json"}`, `{download:sel, value?}`, `{scroll:sel|"bottom"|"top"}`, `{wait:ms}`, `{remember:"name", from:sel, attr?:"href"}` (used as `{{name}}`), `{dialog:"accept"|"dismiss", value?}`, `{offline:true|false}`, `{clock:"+1d"|"-2h"|"2030-01-15T09:00"}`.
- **Assert:** `{waitFor:sel}`, `{expectVisible:sel}`, `{expectHidden:sel}`, `{expectText:"..."}`, `{expectNoText:"..."}`, `{expectUrl:"/path"|"/path#hash"|"/prefix*"}`, `{expectCount:sel, count}`, `{snapshot:sel}` + `{expectChanged:sel}`, `{expectAnimating:true}`, `{expectJs:"<expr>"}`.
- **Beyond the page:** `{request:"POST /api/...", body?, headers?, status?}` + `{expectResponse:{...}|"text"}` + `{remember:"id", from:"response.id"}`; `{expectEmail:"text", to?}` + `{remember:"link", from:"email.link"|"email.code"|"email.body"}`; `{runSchedule:"name"}`; `{webhook:"name", body}`. Flow `users:{admin:{role:"admin", signedIn?:true}}` provisions accounts (`{{email:admin}}`/`{{password:admin}}`). Test checkout uses `#of-checkout-pay` / `#of-checkout-cancel`.
- **Step options & tokens:** `timeout` (ms, default 5000, max 60000), `as:"<name>"`, `in:sel` (supports `:has-text("...")`), `viewport`: `desktop|tablet|mobile|mobile-landscape|tablet-landscape`. Tokens: `{{email}}`, `{{password}}`, `{{username}}`, `{{phone}}`, `{{run}}` (8-char run id), `{{totp:secret}}`.
- **Isolation & stand-ins:** each flow starts with clean browser storage, while server DB records persist across flows until replay rollback (tag created records with `{{run}}`). Replay stubs LLM/agent calls with `"Test reply from the replay model."` (or schema-shaped JSON) — wait for spinners to hide, never assert exact LLM text. Leave audio, QR camera, and pointer-lock mouse-look out of flows.
- **Coverage:** test core user journeys per project shape (DOM CRUD + cross-user privacy, 2-user realtime with `as:"bob"`, canvas HUD changes alongside `expectAnimating`, PWA `offline:true`, mobile viewport). Fix `warnings`/`weak_checks`.
- **Red flows:** fix the app or the flow (never add `window.__x` test globals or replace `of_test.js` with diagnostic flows). After 3 replays with no progress, stop and complete the item.
