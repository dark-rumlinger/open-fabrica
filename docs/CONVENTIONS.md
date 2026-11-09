# Conventions

Lightweight house rules for this repo. The linter enforces formatting and a
curated set of checks (see [.golangci.yml](../.golangci.yml)); the items below are
the conventions it does **not** mechanically enforce, so keep them in mind in
review.

## Go source filenames

Multi-word source filenames use `snake_case`:

```
project_analytics.go      not  projectanalytics.go
service_providers.go      not  serviceproviders.go
cache_key.go              not  cachekey.go
```

Single-word names stay bare (`auth.go`, `render.go`, `worker.go`). Use the
underscore to separate a file into a readable feature name, and to group a
family of files under a shared prefix (`schema_ddl.go`, `schema_index.go`;
`dynamic_api_crud.go`, `dynamic_api_rls.go`; `manage_files.go`,
`manage_secrets.go`).

### Mandatory exceptions

- **Platform build constraints:** `*_windows.go`, `*_unix.go`, `*_darwin.go`,
  `*_linux.go`, `*_other.go`. The suffix is a GOOS/GOARCH build tag; renaming it
  changes what compiles.
- **Test files:** the `_test.go` suffix is required by the toolchain. (The part
  *before* it still follows the rule: `project_files_guard_test.go`.)
- **Tool files match the tool `Name`:** a tool registered as `manage_2fa`
  lives in `manage_2fa.go` (see the tools section below). The name, including
  any digit, mirrors the registered `Name`; leave it.
- **Single-file packages match the package name:** a small utility package whose
  only real file is named after the package keeps that spelling
  (`internal/stringutil/stringutil.go`, `internal/qrcode/qrcode.go`). Go package
  names are single lowercase tokens with no underscores, and the file mirroring
  the package identifier is the stronger convention here.

## Directories and packages

- Directory names are lowercase, no underscores; the package name matches the
  directory (`engine/` → `package engine`).
- Cross-cutting utilities live under `internal/` (import-restricted to this
  module by the Go toolchain). Tool-only helpers live under `tools/internal/`
  (import-restricted to the `tools/` subtree) - this is intentional, not a
  duplicate of the root `internal/`.
- A value stored in the database and read or written by more than one package
  is named once, next to its owner: project modes in `db/models`
  (`models.ModeBuilding` and friends), build state (`of_runs`/`of_tasks` statuses,
  `of_run_log` kinds and stage statuses) in `internal/buildstate`. Do not mirror
  these as local literals.
- What an owner sees for a project is derived, not stored. `projects.mode` (the
  stored intent) and the latest run feed `internal/projectphase`, which yields
  one phase: Building, Needs you, Live, or Paused. The admin API, admin UI, and
  bot all render that value, and none recompute it from mode, run status, or
  engine state. "Live" names a phase only; whether visitors can reach the site
  is the separate `projects.published` flag, and reachability detail
  (HTTPS, domain) is the hosting state. The engine worker is an implementation
  detail with its own Advanced controls.
- Outbound HTTP made on a model's or project's behalf goes through
  `internal/netguard` (URL validation plus SSRF-hardened clients).
- **Tool packages are the model-facing surface, not a library.** Runtime code
  (the public server, jobs, triggers, the engine) never imports a tool category;
  logic both sides need lives in a domain or shared package they each import:
  `billing` (payment tables, webhook verification and reconciliation), `security`
  (crypto, including TOTP), `internal/endpoints` (endpoint kinds, upload limits),
  `internal/netguard`, and `probe` (the headless runtime/flow/WebSocket probes the
  engine's validators and the testing tools share). In the other direction, tools
  never import the engine, the servers, or the runtime that calls them.
  `internal/archtest` (`TestLayering`) enforces all of this.
- Schema belongs in `db/migrations` / `db/project_migrations`, and only there.
  Never create a platform table from Go with `CREATE TABLE IF NOT EXISTS`: the
  migration runs first on every open, so the runtime copy silently never applies
  and any column only it declares is missing in production (this is how `of_jobs`
  lost the `result`/`error` columns the job worker writes).
  `TestMigrationsOwnTheSchema` (`internal/archtest`) enforces it.

## Tools

Every LLM-callable tool is declared in `tools/{category}/manage_{name}.go` and
registers itself in `func init()` via `tools.Define(tools.Tool{...})` (see
[tools/define.go](../tools/define.go)). The file name, the tool `Name`, and the
`Category` (which equals the directory) all agree; `TestToolFileNameCategoryAgree`
enforces it. Category packages load through the single import list in
[tools/all](../tools/all/all.go), which the binary and every test share.

- **One call path.** Every caller (build loop, live chat, scheduled jobs, event
  triggers) runs a tool through `tools.Executor.Invoke` with the shared
  `tools.Env` the composition root builds once. Do not construct a
  `tools.ToolContext` by hand; `Env.ForProject` is the only place one is shaped.
- **Traits over name lists.** Build-loop policy that depends on what a tool is
  (`Probe`, `RealtimeModel`, `Stages`, `MaxDuration`, `RequiresCapability`) is
  declared on the tool, not listed by name in the engine.
- **Tests use `tools/tooltest`.** It runs tools against fully migrated databases
  through the production `Invoke` path. Hand-written `of_*` DDL in a tool test
  fails `TestNoHandWrittenPlatformDDLInToolTests`.

- **`find_tool`** is the built-in tool-discovery meta-tool; it is the one
  LLM-callable tool that intentionally does not carry the `manage_` prefix.
  Every domain tool follows `manage_{name}` with no exceptions.
- **Optional `Tool` fields** (`Hints`, `Summarize`, `MaxResult`, and the rest of
  the runtime hooks in [tools/types.go](../tools/types.go)) are genuinely optional.
  `MaxResult` defaults to the engine's 3000-byte session cap. Set `Hints`/`Summarize` when a tool's output
  benefits from post-run guidance or custom summarization; set `MaxResult` when a
  tool can return large results and the default cap is wrong. Absence is not a
  defect.

## Messages for people

Text the Go side writes for a person is English in the source and translated by
`internal/msgcat`; handlers do not branch on language.

- **API errors.** Keep writing `writeError(w, status, "project not found")`. The
  admin router translates the message into the language the admin UI reports in
  its `X-Of-Locale` header. A template (`"failed to save %s"`, or a literal
  prefix plus a value) is matched as a template.
- **What the engine says in chat.** Pass the line to `publishMessage`, which
  translates it into the project's reply language. To build a line from parts,
  translate the parts with `w.tr(...)`. Text a model wrote, text the chat UI
  recognises by its English wording, and the go-live summary go out with
  `publishVerbatim`.
- **The Telegram bot.** Everything `bot/` says to a person goes through a
  `voice` (`s.voice(state)`, or `s.voiceForUser(id)` before a session exists):
  `v.tr("...")` for plain text such as buttons and toasts, `v.trh("...")` for HTML
  messages, which escapes string and error arguments. The language is the
  project's reply language when its owner picked one, else the one the person's
  Telegram app reports (remembered per allowed user). A message that goes to
  several people is built per recipient (`broadcastRender`, `alertOthers`), and
  `setMyCommands` / the profile text are published once per catalog language.
  Numbers must be formatted before they are passed (`%s`, never `%.1f`), and a
  message must not start or end with whitespace, so put newlines outside it.
- **A message written in one place and shown from another** (a table of lines, a
  message a function returns) is wrapped in `msgcat.Mark("...")`.
- **Catalogs** are `internal/msgcat/locales/<code>.json`, keyed by the English
  message. `internal/msgcat/coverage_test.go` reads the source for every message
  and fails until each catalog has it (same arguments, no stale entries). Run
  `MSGCAT_DUMP=<file> go test ./internal/msgcat -run TestDumpMessages` for the
  current list. Changing an English message is a catalog change too.
- **Figures in the admin UI** (numbers, sizes, money, durations) go through
  `web/admin/app/core/format.js`, never `toFixed` or a hard-coded `$` / `KB`, so
  separators, symbols and unit names follow the interface language.
- **Enter that submits** uses `isSubmitEnter` from `ui/a11y.js`, so an input
  method (Japanese, Chinese, Hindi) confirming a candidate does not also send.
- **Never translated:** prompts, tool schemas and descriptions, tool results, and
  anything else a model reads. Those are contracts and stay English. So does text
  that comes from a library, the database or the operating system.

## Logging

Use the standard library `log/slog` for all logging, with structured key/value
pairs (`slog.Error("msg", "error", err)`), not `fmt`/`log` printing.

The single exception is `tlsproxy/`, which passes `zap.NewNop()` to satisfy
CertMagic's `*zap.Logger`-typed `Logger` fields; TLS lifecycle is logged from the
package's own slog wrapper. Do not introduce `go.uber.org/zap` anywhere else.

## Randomness

Anything a user could guess their way past uses `crypto/rand`: session and magic-link
tokens, TOTP seeds, the JWT and at-rest encryption keys, CSP nonces, webhook
secrets, and upload filenames. `math/rand` is for non-security variety and
timing only, and its current call sites are the whole sanctioned list: retry
jitter (`internal/sqliteretry/retry.go`, `engine/retry.go`), generated project names
and slug tokens (`internal/projectident/projectident.go`), build-status phrasing
(`engine/publish.go`),
and the sandbox's `random()` exposed to model-authored functions
(`internal/funcsandbox/sandbox.go`).

Nothing mechanically enforces this: the blocking `gosec` job excludes G404 for
exactly the sites above (see [.github/workflows/ci.yml](../.github/workflows/ci.yml)),
so a new `math/rand` token would pass CI. Reach for `crypto/rand` by default and
leave `math/rand` to the four cases named here.

## Brand spelling

The name is spelled by context, consistently:

- **`Open Fabrica`** (two words) in prose, docs, and user-facing strings.
- **`OpenFabrica`** (one word) in Go identifiers and type names.
- **`openfabrica`** (lowercase) for the binary, database file, and as the
  `OPENFABRICA_` environment-variable prefix.

Product/author attribution (the startup banner and README) is intentional and
kept. Do not embed the personal author name in output shipped to end users of
generated projects (e.g. page `<meta generator>`); use `Open Fabrica` alone.
