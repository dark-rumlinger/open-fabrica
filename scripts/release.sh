#!/bin/bash
set -e

# This script lives in scripts/ but every path below is relative to the repo
# root, so anchor there regardless of where it is invoked from.
cd "$(dirname "$0")/.."

# Read version from cmd/openfabrica/main.go (single source of truth)
VERSION="v$(awk -F'"' '/var Version =/{print $2; exit}' cmd/openfabrica/main.go)"
if [ "$VERSION" = "v" ]; then
  echo "Error: could not read version from cmd/openfabrica/main.go"
  exit 1
fi
BINARY="openfabrica"
EXTRAS="config.json seed.json LICENSE THIRD_PARTY_LICENSES"
WIN_EXTRAS="start.bat"
UNIX_EXTRAS="start.sh"
SCRIPTS_DIR="scripts" # launch scripts live here; they ship at the archive root
OUTDIR="release"
STAGING="release/staging/OpenFabrica"

# sedi edits a file in place on both GNU and BSD sed (macOS needs a backup suffix).
sedi() {
  local file="${@: -1}"
  sed -i.bak "$@" && rm -f "$file.bak"
}

rm -rf "$OUTDIR"
mkdir -p "$STAGING"

# Parse semver (strip leading v)
SEMVER="${VERSION#v}"

BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# main.Version carries the bare semver; the banner and About dialog add the "v".
LDFLAGS="-s -w -X main.Version=$SEMVER -X main.BuildTime=$BUILD_TIME"

MAJOR="${SEMVER%%.*}"
REST="${SEMVER#*.}"
MINOR="${REST%%.*}"
PATCH="${REST#*.}"
PATCH="${PATCH%%-*}" # strip any pre-release suffix

# Patch cmd/openfabrica/versioninfo.json with release version
sedi "s/\"Major\": [0-9]*/\"Major\": $MAJOR/g" cmd/openfabrica/versioninfo.json
sedi "s/\"Minor\": [0-9]*/\"Minor\": $MINOR/g" cmd/openfabrica/versioninfo.json
sedi "s/\"Patch\": [0-9]*/\"Patch\": $PATCH/g" cmd/openfabrica/versioninfo.json
sedi "s/\"FileVersion\": \"[^\"]*\"/\"FileVersion\": \"$VERSION\"/" cmd/openfabrica/versioninfo.json
sedi "s/\"ProductVersion\": \"[^\"]*\"/\"ProductVersion\": \"$VERSION\"/" cmd/openfabrica/versioninfo.json

# Regenerate embedded assets before any build: the encrypted knowledge tree and the
# minified admin UI + client helpers. The generators locate the repo root themselves
# and are deterministic, so this is safe to run every time. set -e aborts the release
# if either fails rather than shipping stale or cleartext embeds.
echo "Regenerating embedded assets..."
go run openfabrica/knowledge/encgen
go run openfabrica/internal/assetgen

echo "Building $VERSION..."

# Copy extras into the Open Fabrica directory inside staging.
# config.json holds NO secrets - the JWT/encryption keys live in env vars and the
# data/.security key files. The guard below still aborts the release if one of
# SECRET_FIELDS ever appears populated in the shipped file.
SECRET_FIELDS="jwt_secret encryption_key"

# The shipped config.json is a fixed minimal file, never the builder's own
# (gitignored) local config.json: every omitted key falls back to the built-in
# default, and config.example.json documents the full set.
write_release_config() {
  cat > "$1" <<'JSON'
{
  "admin_port": 5734,
  "public_port": 5733,
  "data_dir": "./data",
  "log_level": "info"
}
JSON
}

for f in $EXTRAS; do
  if [ "$f" = "config.json" ]; then
    write_release_config "$STAGING/config.json"
    continue
  fi
  [ -f "$f" ] || continue
  cp "$f" "$STAGING/"
done

# Fail closed: never ship a config.json that carries a populated secret, even if
# write_release_config is later changed to include one.
if [ -f "$STAGING/config.json" ]; then
  for f in $SECRET_FIELDS; do
    if grep -Eq "\"$f\"[[:space:]]*:[[:space:]]*\"..*\"" "$STAGING/config.json"; then
      echo "Error: release config.json still has a non-empty \"$f\"; aborting." >&2
      exit 1
    fi
  done
fi

# Windows AMD64 (.zip, native for Windows users)
echo "  windows/amd64"
go generate ./cmd/openfabrica
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$STAGING/$BINARY.exe" ./cmd/openfabrica
for f in $WIN_EXTRAS; do
  [ -f "$SCRIPTS_DIR/$f" ] && cp "$SCRIPTS_DIR/$f" "$STAGING/"
done
if command -v zip >/dev/null 2>&1; then
  (cd release/staging && zip -qr "../OpenFabrica_${VERSION}_windows_amd64.zip" OpenFabrica)
else
  powershell -NoProfile -Command "Compress-Archive -Path 'release/staging/OpenFabrica' -DestinationPath 'release/OpenFabrica_${VERSION}_windows_amd64.zip'"
fi
rm "$STAGING/$BINARY.exe"
for f in $WIN_EXTRAS; do rm -f "$STAGING/$f"; done

# Add unix extras for Linux/macOS builds.
for f in $UNIX_EXTRAS; do
  [ -f "$SCRIPTS_DIR/$f" ] && cp "$SCRIPTS_DIR/$f" "$STAGING/"
done

# Linux AMD64
echo "  linux/amd64"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$STAGING/$BINARY" ./cmd/openfabrica
tar -czf "$OUTDIR/OpenFabrica_${VERSION}_linux_amd64.tar.gz" -C release/staging OpenFabrica/
rm "$STAGING/$BINARY"

# Linux ARM64
echo "  linux/arm64"
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "$STAGING/$BINARY" ./cmd/openfabrica
tar -czf "$OUTDIR/OpenFabrica_${VERSION}_linux_arm64.tar.gz" -C release/staging OpenFabrica/
rm "$STAGING/$BINARY"

# macOS AMD64
echo "  darwin/amd64"
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$STAGING/$BINARY" ./cmd/openfabrica
tar -czf "$OUTDIR/OpenFabrica_${VERSION}_darwin_amd64.tar.gz" -C release/staging OpenFabrica/
rm "$STAGING/$BINARY"

# macOS ARM64
echo "  darwin/arm64"
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "$STAGING/$BINARY" ./cmd/openfabrica
tar -czf "$OUTDIR/OpenFabrica_${VERSION}_darwin_arm64.tar.gz" -C release/staging OpenFabrica/
rm "$STAGING/$BINARY"

# Generate SHA-256 checksums for every archive so downloaders can verify
# integrity. Written from inside $OUTDIR so the file lists bare archive names
# (verify later with: `sha256sum -c SHA256SUMS`).
echo "  checksums"
(
  cd "$OUTDIR"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum OpenFabrica_*.zip OpenFabrica_*.tar.gz > SHA256SUMS
  else
    shasum -a 256 OpenFabrica_*.zip OpenFabrica_*.tar.gz > SHA256SUMS
  fi
)

# Optionally sign the checksums file for authenticity (not just integrity).
# Opt-in: set OF_MINISIGN_KEY to a minisign secret-key path to emit
# SHA256SUMS.minisig. Without it, releases ship with checksums only.
if [ -n "${OF_MINISIGN_KEY:-}" ] && command -v minisign >/dev/null 2>&1; then
  echo "  signing checksums (minisign)"
  minisign -S -s "$OF_MINISIGN_KEY" -m "$OUTDIR/SHA256SUMS"
fi

# Clean up staging
rm -rf release/staging

# Restore cmd/openfabrica/versioninfo.json to dev defaults (no-op if not a git repo or unchanged,
# so the release still finishes cleanly rather than aborting under `set -e`).
git checkout cmd/openfabrica/versioninfo.json 2>/dev/null || true

echo ""
echo "Done! Release files in $OUTDIR/:"
ls -lh "$OUTDIR/"
