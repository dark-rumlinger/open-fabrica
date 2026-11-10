VERSION ?= $(shell awk -F'"' '/^var Version = / {print $$2}' cmd/openfabrica/main.go)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -s -w -X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)
BINARY = openfabrica
# Host executable suffix: ".exe" on Windows, empty on Linux/macOS. Keeps the
# default build/run targets correct off Windows instead of hardcoding ".exe".
GOEXE := $(shell go env GOEXE)

# Parse semver for cmd/openfabrica/versioninfo.json patching
SEMVER = $(subst v,,$(VERSION))
VER_MAJOR = $(word 1,$(subst ., ,$(SEMVER)))
VER_MINOR = $(word 2,$(subst ., ,$(SEMVER)))
VER_PATCH = $(firstword $(subst -, ,$(word 3,$(subst ., ,$(SEMVER)))))

.PHONY: patch-version restore-version

# One sed pass written to a temp file and moved into place: `sed -i` differs between
# GNU and BSD (macOS) sed, so it is avoided here.
patch-version:
	@sed \
	  -e 's/"Major": [0-9]*/"Major": $(or $(VER_MAJOR),0)/g' \
	  -e 's/"Minor": [0-9]*/"Minor": $(or $(VER_MINOR),0)/g' \
	  -e 's/"Patch": [0-9]*/"Patch": $(or $(VER_PATCH),0)/g' \
	  -e 's/"FileVersion": "[^"]*"/"FileVersion": "$(VERSION)"/' \
	  -e 's/"ProductVersion": "[^"]*"/"ProductVersion": "$(VERSION)"/' \
	  cmd/openfabrica/versioninfo.json > cmd/openfabrica/versioninfo.json.tmp
	@mv cmd/openfabrica/versioninfo.json.tmp cmd/openfabrica/versioninfo.json

restore-version:
	@git checkout cmd/openfabrica/versioninfo.json 2>/dev/null || true

.PHONY: generate build build-linux build-darwin build-windows build-all clean clean-data run dev

# Regenerate the embedded assets: the encrypted knowledge tree (knowledge/encrypted/)
# and the minified admin UI + client helpers (web/admin-min/, *.min.js). These are
# build artifacts and are NOT committed (see .gitignore), so run this once after
# cloning. Every build, test, vet and dev target depends on it because the go:embed
# directives fail to compile without the generated files. Output is deterministic,
# so this rewrites nothing when the sources are unchanged.
generate:
	go generate ./knowledge ./web

build: patch-version generate
	@if [ "$$(go env GOOS)" = "windows" ]; then go generate ./cmd/openfabrica; fi
	go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY)$(GOEXE) ./cmd/openfabrica
	@$(MAKE) restore-version

build-linux: generate
	GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 ./cmd/openfabrica
	GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 ./cmd/openfabrica

build-darwin: generate
	GOOS=darwin GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-amd64 ./cmd/openfabrica
	GOOS=darwin GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-arm64 ./cmd/openfabrica

build-windows: patch-version generate
	go generate ./cmd/openfabrica
	GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-windows-amd64.exe ./cmd/openfabrica
	@$(MAKE) restore-version

build-all: build-linux build-darwin build-windows

run: build
	./bin/$(BINARY)$(GOEXE)

dev: generate
	go run -tags dev ./cmd/openfabrica

# Remove build output only. NEVER touches data/ - projects, databases, and the
# encryption/JWT keys live there (losing encryption.key is unrecoverable; see the
# README). Run `make clean-data` to wipe local data on purpose.
clean:
	rm -rf bin/ release/

# Destructive: wipe ALL local data (every project, database, and your encryption/
# JWT keys). Gated behind a typed confirmation so it is never run by reflex.
clean-data:
	@printf 'Delete data/ (every project, DB, and your encryption/JWT keys)? Type YES: '; \
		read ans; \
		if [ "$$ans" = "YES" ]; then rm -rf data/ && echo "data/ removed."; else echo "Aborted."; fi

.PHONY: test test-js test-race vet fmt fmt-check lint check

# Run the full test suite.
test: generate
	go test ./...

# Tests for the admin frontend (web/). Uses Node's built-in runner and assert -
# no framework, no package manager, nothing to install. The package.json under
# web/ is a `"type": "module"` marker, not a dependency manifest.
test-js:
	@command -v node >/dev/null 2>&1 || { \
		echo "node not installed (needed for the web/ tests): https://nodejs.org/"; exit 1; }
	node --test "web/tests/*.test.js"

# Race detector. Needs cgo and a C toolchain, which the normal build
# deliberately does not (every dependency is pure Go, so the binary
# cross-compiles without one). Not part of `check` for that reason.
test-race: generate
	CGO_ENABLED=1 go test -race ./...

# Static analysis (built into the toolchain, no extra install).
vet: generate
	go vet ./...

# Rewrite source to gofmt canonical form.
fmt:
	gofmt -w .

# Fail if any file is not gofmt-clean (for CI).
fmt-check:
	@unformatted=$$(gofmt -l . | grep -v '^data/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

# Full linter (requires golangci-lint; see .golangci.yml).
lint: generate
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run

# One-shot gate: format check, vet, build, and tests (Go + web).
check: fmt-check generate vet
	go build ./...
	go test ./...
	@$(MAKE) test-js
