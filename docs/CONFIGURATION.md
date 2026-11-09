# Configuration

Open Fabrica reads configuration from three places. This document explains how
they combine and where each kind of setting lives, so it is clear which knob to
turn for a given change. For the full list of individual keys and their defaults,
see `config.example.json` and the Configuration table in the README.

## The three sources

1. **Built-in defaults** (`config/defaults.go`). Every setting has a safe default,
   so the app runs with no config file and no environment variables.
2. **Config file** (`config.json` by default; override with `--config <path>`).
   Any key present overrides the default. A missing file is not an error, but a
   file that exists and cannot be read or parsed is, rather than quietly running
   on defaults.
3. **Environment variables** (prefixed `OPENFABRICA_`). These override both the
   defaults and the config file.

### Precedence (config-time settings)

```
built-in default  <  config file  <  environment variable
```

This applies to the infrastructure-level settings resolved once at startup:
ports, bind address, data directory, log level, TLS flags, rate limits, LLM
timeout, and the engine stage timeouts. Environment variables are the config key
in upper case with an `OPENFABRICA_` prefix, with a few shortened names, so treat
this table as the authority:

| Config file key       | Environment variable            | Default       |
| --------------------- | ------------------------------- | ------------- |
| `admin_port`          | `OPENFABRICA_ADMIN_PORT`        | `5734`        |
| `public_port`         | `OPENFABRICA_PUBLIC_PORT`       | `5733`        |
| `admin_bind_addr`     | `OPENFABRICA_ADMIN_BIND`        | `127.0.0.1`   |
| `data_dir`            | `OPENFABRICA_DATA_DIR`          | `./data`      |
| `log_level`           | `OPENFABRICA_LOG_LEVEL`         | `info`        |
| `log_file`            | `OPENFABRICA_LOG_FILE`          | none (console only) |
| `public_csp`          | `OPENFABRICA_PUBLIC_CSP`        | permissive    |
| `trusted_proxies`     | `OPENFABRICA_TRUSTED_PROXIES`   | none (loopback only) |

`trusted_proxies` is a list of IPs or CIDRs (comma-separated in the environment variable) of reverse proxies in front of the public server, beyond the loopback proxy that is always trusted. Set it when a proxy off this host (a container network, a load balancer) fronts the server, so rate limits and IP bans see the real visitor. The client address is the first address in `X-Forwarded-For`, read from the right, that is not itself a trusted proxy.

Boolean variables read `1`, `true`, `yes` or `on` as true; `0` and `false` are
off. A numeric variable that does not parse is ignored with a warning in the log.
`OPENFABRICA_MEMORY_LIMIT` (a soft heap ceiling such as `1GiB`; `GOMEMLIMIT` is
honored when it is unset) is also environment-only, see `docs/SECURITY.md`.

A few settings are environment-only (no config-file key), such as
`OPENFABRICA_ADMIN_EXPOSE` and `OPENFABRICA_ALLOW_KEY_REGEN`. The
`_env_only_settings` block in `config.example.json` documents these.

## The runtime exception: engine daily token budget

One setting does not follow the config-time model, because an operator may want
to change it while the server is running (from the admin UI) without a restart.
The engine daily token budget is resolved live on each use with a different order:

```
environment variable  >  DB admin setting  >  config file  >  default
```

So `OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET`, if set, always wins; otherwise the
value saved in the admin UI wins; otherwise `engine_daily_token_budget` from the
config file; otherwise the built-in default. See
`resolveEngineDailyTokenBudget` in `cmd/openfabrica/main.go`.

## Settings stored only in the database

Some settings are entered through the admin UI and stored in the database (the
provider secrets encrypted at rest). They have no config-file key or environment
variable, because they are per-installation state set after first-run setup:

- **LLM / image / search provider API keys** (entered in the setup wizard,
  stored encrypted).
- **Telegram bot token** (stored encrypted, never sent back to the browser) and
  **admin base URL** (edit it under System Settings; the messaging bot uses it for "Open in admin" links and hides them while it is blank).
- **Per-project public CSP override** (admin -> project Settings -> Advanced),
  which overrides the instance-wide `public_csp` for that one project.

## Telegram bot

Set it up from the admin panel under Messaging Apps:

1. Create a bot with @BotFather and paste its token. The token is checked with
   Telegram straight away.
2. Scan the QR code, or tap "Open in Telegram", and press Start. The invite link
   works once and expires after 15 minutes; it adds your Telegram account to the
   allowlist, so there is no user ID to look up.
3. Use "Add someone" for each further person. Everyone on the list has full
   control of your projects from Telegram.

Stored in the database (all managed by that page; there is nothing to edit by
hand): `telegram_bot_token` and `telegram_allowed_users`. The bot only answers
private chats. A stranger who writes to it gets a rate-limited reply with their
own Telegram user id (so you can add them) and nothing else; only people on the
allowlist can do anything.

In Telegram, each person can choose how chatty the bot is with /alerts: Quiet
(replies, questions and errors), Normal (adds a live status card that updates
while the builder works) or Verbose (adds a line per LLM call). /log lists every
LLM call with its tokens, timing and cost, and can send the full record of one
call as a file. Project links are only offered for projects with a real public
domain; `*.localhost` addresses only work on the host computer.

## Secrets and key files

- The **JWT signing key** and **at-rest encryption key** auto-generate on first
  run into `data/.security/jwt.key` and `data/.security/encryption.key`
  (mode 0600). Prefer these key files over putting secrets in config or
  environment variables. `OPENFABRICA_JWT_SECRET` and
  `OPENFABRICA_ENCRYPTION_KEY` (also the `jwt_secret` and `encryption_key` config
  keys) exist to override them when you must supply your own, for example in a
  secrets manager. The encryption key is exactly 64 hex characters; a trailing
  newline in a key file is ignored.
- Provider API keys live in the database, encrypted with the at-rest key.

Losing `encryption.key` makes previously-encrypted secrets permanently
unreadable, and replacing `jwt.key` invalidates every session. Regenerating a
missing key on an existing database requires the explicit
`OPENFABRICA_ALLOW_KEY_REGEN=1` opt-in.

## Backup and restore

```bash
openfabrica backup                  # writes openfabrica-backup-<timestamp>.zip
openfabrica backup my-backup.zip
openfabrica restore my-backup.zip   # into an empty instance; stop the server first
openfabrica restore my-backup.zip --force   # replace an existing instance
```

A backup is one zip holding the core database, every project database, uploaded
and generated files, the key files and the ACME certificates. SQLite files are
captured with `VACUUM INTO`, so `backup` is safe while the server runs. `restore`
needs the server stopped, refuses to write over an existing instance without
`--force`, and re-anchors stored file paths, so a backup restores correctly into a
different data directory or machine. It unpacks into a staging directory inside
the data directory first and swaps in only after every entry validates, so a bad
or truncated archive does not damage the existing instance. The archive contains
the encryption and signing keys: store it as carefully as the data directory.

The admin panel can make the same archive without a terminal: System Settings has
a **Download backup** button (`GET /admin/api/system/backup`, admin only). Restore
stays a command-line step because it replaces files the running server holds open.

## Logs

Logs go to the console. Set `log_file` (or `OPENFABRICA_LOG_FILE`) to also write
them to a file: a relative path is inside the data directory, the file rotates at
20 MB, and five old files are kept. The latest 2000 lines are also held in memory
and served to admins at `GET /admin/api/system/logs?lines=300`, so recent
activity can be read without a terminal; System Settings also shows them under
**Server logs**.

## Locked out of the admin panel

```bash
openfabrica reset-admin            # the only admin
openfabrica reset-admin alice      # a named user
```

Sets a new random password, prints it once, and signs that user out everywhere.
It needs access to the data directory, which is what proves you own the instance.
If there are two or more admins you must name the user.

## Getting started

You do not need a config file. Either:

- Run with defaults and set anything you need via `OPENFABRICA_*` environment
  variables, then complete provider setup in the admin wizard; or
- Copy `config.example.json` to `config.json` and edit the keys you care about.

`config.json` is listed in `.gitignore` so a local file with real values is not
committed, and `config.example.json` is the template. If `config.json` is still
tracked from an earlier commit, untrack it once with `git rm --cached config.json`
(this keeps your local working file).
