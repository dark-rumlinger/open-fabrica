# Contributing to Open Fabrica

Thanks for your interest in improving Open Fabrica. This guide covers building
from source, the local quality gates, and what a good change looks like.

## Prerequisites

- **Go 1.26+** (see the `go` directive in [go.mod](../go.mod)).
- A C compiler is **not** required, the SQLite driver is pure Go
  (`modernc.org/sqlite`), which is what lets Open Fabrica ship as a single
  cross-compiled binary.
- **Node** (any recent version) - `make check` runs the `web/` tests through Node's
  built-in test runner, and the target fails outright when `node` is not on PATH.
  Nothing to install beyond Node itself: `web/` ships as ES modules with no
  framework and no package manager.
- Optional, only for specific features/checks:
  - [golangci-lint](https://golangci-lint.run/welcome/install/) - for `make lint`.
  - A Chromium-based browser (Chrome/Chromium/Edge) - for the PDF and scrape
    tools and the headless-Chrome runtime probe used by some tests.

## Build from source

```bash
git clone https://github.com/dark-rumlinger/open-fabrica.git
cd open-fabrica

# The minified admin UI, encrypted knowledge tree and *.min.js helpers are build
# artifacts (git-ignored). Generate them once after cloning; make dev, make build,
# make test and make check run this for you.
go generate ./knowledge ./web      # or: make generate

# Fast path: run directly. `make dev` passes `-tags dev`, which serves the
# admin UI from web/admin on disk, so edits show on reload without a rebuild.
make dev
# or (embedded admin UI, needs a rebuild to pick up admin changes):
go run ./cmd/openfabrica

# Produce a stripped, version-stamped binary in ./bin.
make build            # host platform
make build-all        # linux and darwin (amd64/arm64), windows (amd64)
```

Then open `http://localhost:5734` and follow the setup wizard. See the
[README](../README.md#configuration) for configuration and the
[Architecture](../README.md#architecture) section for how the engine is wired.

## Quality gates (run before every PR)

CI runs on every push and pull request to `main`
([.github/workflows/ci.yml](../.github/workflows/ci.yml)): it mirrors `make check`
(gofmt + `go vet` + build + test + the `web/` tests) and adds the full `golangci-lint` pass, a
blocking `govulncheck`, and a `gosec` scan. Please run the same gates locally
before opening a pull request so review starts from green:

```bash
make check            # gofmt check + go vet + go build ./... + go test ./... + web/ tests
make test             # go test ./...
make lint             # golangci-lint (see .golangci.yml)
```

Individual targets: `make fmt` (rewrite to gofmt canonical form),
`make fmt-check`, `make vet`. A change should build cleanly, keep `go vet` and
the linter quiet, and keep the test suite green. New behavior needs new tests.

Two safety nets worth knowing before refactoring the engine or the tools:

- **Golden prompt bytes** (`engine/testdata/golden`, `TestGoldenPromptBytes`)
  pin every request an offline EXPAND → PLAN → BUILD → LIVE run sends. Provider
  prompt caches key on those bytes, so an unexplained diff is a cost regression.
  After an intended prompt change, regenerate with
  `go test ./engine -run TestGoldenPromptBytes -update` and review the diff.
- **`tools/tooltest`** runs tools through the production `Executor.Invoke` path
  against fully migrated databases; write tool tests with it rather than
  hand-built schemas.

## Conventions

- Read [CONVENTIONS.md](CONVENTIONS.md) for the house rules the linter does not
  mechanically enforce (file naming, package layout, tool registration).
- LLM-callable tools live in `tools/{category}/manage_{name}.go` and self-register
  in `func init()`; the file name, tool `Name`, and `Category` must agree
  (see [tools/AUTHORING.md](../tools/AUTHORING.md)).
- Load-bearing cross-package facts are defined once in `contracts/` and
  transcluded, do not copy them; the drift/guard tests will fail the build on a
  divergent copy.
- Prefer small, focused commits with a clear message describing the *why*.

## Reporting bugs and requesting features

Open an issue on GitHub with steps to reproduce (and, for build issues, your OS,
Go version, and provider). For **security** issues, do **not** open a public
issue, follow [SECURITY.md](SECURITY.md) instead.

## License

The project is distributed under the [MIT License](../LICENSE).
By contributing you agree that your contribution is licensed under the same MIT License.
Only contribute work that you wrote or have the right to submit.
