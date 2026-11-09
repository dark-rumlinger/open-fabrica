# Security Policy

Open Fabrica runs and stores everything on the operator's own machine, and the
admin control plane can read every project's data and decrypt stored secrets. We
take vulnerability reports seriously.

## Supported versions

Open Fabrica is in early, pre-1.0 development. Security fixes are applied to the
**latest release** only. Please make sure you can reproduce an issue on the most
recent [release](https://github.com/dark-rumlinger/open-fabrica/releases) (or `main`)
before reporting.

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

Report privately using **GitHub's private vulnerability reporting**:

1. Go to the repository's **Security** tab → **Report a vulnerability**
   (Security Advisories).
2. Include: affected version/commit, a description of the issue and its impact, a
   proof-of-concept or reproduction steps, and any suggested remediation.


### What to expect

- We aim to acknowledge a report within a few days.
- We will confirm the issue, keep you updated on remediation, and credit you in
  the release notes unless you prefer to remain anonymous.
- Please give us a reasonable window to ship a fix before any public disclosure.

## Scope and known trade-offs

Some behaviors are **deliberate design choices**, documented in the README's
["Before You Go Live"](../README.md#before-you-go-live) section, and are not
vulnerabilities on their own, though reports that show a concrete bypass or
escalation are welcome:

- **The admin panel binds to `127.0.0.1` by default.** Exposing it to a network
  takes two steps: a non-loopback `OPENFABRICA_ADMIN_BIND` (or `admin_bind_addr`)
  and `OPENFABRICA_ADMIN_EXPOSE=1` to acknowledge it, or the server refuses to
  start. It is a local control plane, not a public surface. While it is bound to
  loopback, requests must also carry a `localhost` or loopback-IP `Host` header
  (this stops DNS-rebinding pages and same-host reverse proxies from reaching it),
  and the unauthenticated build-triage endpoint additionally refuses any request
  with forwarding headers. Once exposed, every route requires a login, and while
  no admin account exists the first-run setup endpoints require a one-time
  token printed to the server console (the `setup_token` log field), so the first
  stranger to reach an unclaimed instance cannot become its admin.
- **The default public CSP is permissive**, because generated apps are
  unpredictable. Serve untrusted or multi-user apps on their own
  domain/subdomain and tighten the CSP (`OPENFABRICA_PUBLIC_CSP` or per project).
- **The at-rest encryption key (`data/.security/encryption.key`) is the operator's
  responsibility to back up and protect.** Losing it makes stored secrets
  unrecoverable; leaking it exposes them. Back up the whole data directory
  together with it, including `data/certs` (the ACME account and issued
  certificates) and `data/.security`.
- **Any paired Telegram user can erase the whole installation.** `/selfdestruct`
  asks twice (a warning, then a one-time button that expires after 60 seconds)
  and then deletes the keys first, followed by the databases, the data
  directory, the config and backup files, the firewall rule, and the program
  itself. `openfabrica selfdestruct --dry-run` lists what it would remove
  without deleting anything. It cannot reach what lives off the machine:
  Telegram keeps the chat history, certificate transparency logs keep issued
  certificate names, DNS records stay, and off-box backups are untouched.
  Overwriting before deletion is best effort on SSDs and journaling or
  copy-on-write filesystems, so the early key removal is the real protection. In
  Docker the container image and data volume must be removed from the host
  (`docker rm`, `docker rmi`, `docker volume rm`). Only pair people you would trust
  with this.
- **The JS function/room sandbox bounds CPU and output, but not heap.**
  Model-authored handlers run in an embedded JS engine (goja) that enforces a
  wall-clock timeout and caps on response/write/query size, but goja exposes no
  per-VM memory limit and its interrupt can't preempt a single large allocation.
  A runaway or hostile handler that allocates without bound (e.g. an unbounded
  accumulating loop) can exhaust process memory and crash the whole instance,
  every project on it, and Go's out-of-memory is not recoverable in-process.
  Set a soft ceiling with **`OPENFABRICA_MEMORY_LIMIT`** (e.g. `1GiB`) so the Go
  runtime reclaims aggressively as it approaches the limit; when unset a
  `GOMEMLIMIT` in the environment is still honored. That is only a floor. For a
  hard guarantee, run the binary under an **OS/container memory limit with
  automatic restart** (the shipped [`Dockerfile`](../Dockerfile) wires `--memory`
  + `GOMEMLIMIT`; on bare metal use systemd `MemoryMax=` + `Restart=`, or a
  cgroup). This matters most when you run untrusted or multi-tenant apps.
- **Router port forwarding is opt-in and limited to TCP 80 and 443.** The Go
  Live card's "Open ports on my router" asks the local router (UPnP IGD,
  NAT-PMP, or PCP) to forward only the two ports the HTTPS proxy serves; the
  admin port and the plain-HTTP public port are never forwarded. Forwards use
  short leases renewed while the process runs, are removed on shutdown and on
  "Stop forwarding" (Project → Settings → General), and an existing forward is
  only replaced when it was created by Open Fabrica. UPnP discovery replies are
  unauthenticated, so the description and control URLs are only fetched from
  the private address that answered.
- **A password-registered account's email address is not verified.** Registration
  does not email a confirmation, and signing in by magic link or OAuth matches an
  existing account by its email. So if one auth table offers both a password and
  magic-link or OAuth sign-in, someone can pre-register another person's address
  with a password they chose, and that person's later magic-link sign-in lands in
  the account the first party can also enter. Either offer one sign-in method per
  table, or have the project confirm an address before it grants anything valuable.
- **Inbound webhooks are replay-protected only for senders that opt in.** The
  default signature covers the body alone, so a captured, validly signed request
  can be sent again. A sender that adds an `X-Webhook-Timestamp` header (unix
  seconds) and signs `<timestamp>.<body>` instead is rejected outside a 5-minute
  window. Stripe payment webhooks are protected (5-minute timestamp window plus
  event-id deduplication); a project's own custom inbound webhooks that cannot send
  a timestamp should make their handlers idempotent.

- **A project's end-user sessions end on the events that matter, not on every
  possible change.** Sign-in tokens for the apps people build here are stateless
  and last 24 hours by default (an auth endpoint's `jwt_expiry_hours` shortens
  that), but each project records, per user, an instant before which tokens are
  refused. Every token check in the public server honours it, and it is set when a
  password reset completes, when an account row is deleted, and when an update
  changes an account's password, role or a standing flag (`banned`, `is_active`,
  `is_admin`, `verified` and similar), whether through the app's own API or the
  admin table editor. Editing a display name signs nobody out. What this does not
  cover: a WebSocket or stream that is already connected stays open until it next
  reconnects; a change made by other means (a build-time tool, SQL run by a
  trigger or scheduled task) revokes nothing, so after one of those, set the
  account's password through the normal paths or wait out the token lifetime; and
  the revocation is keyed by user id within a project, so two auth endpoints on
  different tables that share an id are revoked together. The admin panel's own
  sessions are separate: changing a password or choosing "Sign out other devices"
  in the Account dialog ends them at once.
- **Webhook signing secrets are encrypted at rest** with the instance encryption
  key, like the project's other secrets, and project exports blank them. A copy of
  the database alone does not reveal them; a copy of the data directory (which holds
  the key) does, so keep backups, which include the key, as carefully as the
  directory itself.
- **Exported CSV files neutralise spreadsheet formulas.** A cell that starts with
  `=`, `+`, `-` or `@` (and is not a plain number) is prefixed with a quote, so a
  value a visitor submitted cannot run as a formula when the owner opens an export.

Things we always want to hear about: authentication/authorization bypasses,
injection (SQL, command, template), SSRF that reaches internal networks, secret
disclosure, sandbox escapes in the JS function runtime, and any way one project
can read or write another's data across an origin boundary.
