#!/usr/bin/env bash
# build-release.sh — build release archives for every supported platform.
#
#   scripts/build-release.sh [version]        # e.g. scripts/build-release.sh v0.1.0
#
# rca is pure Go with no cgo and no embedded native artifacts, so a single host
# cross-compiles all four targets. Archives land in ./dist together with
# checksums.txt.
#
# Archive names carry no version (rca_darwin_arm64.tar.gz) so install one-liners
# can use GitHub's releases/latest/download/ URLs; the version is stamped inside
# the binary (`rca version`) and on the release tag.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

VERSION="${1:-dev-$(git rev-parse --short HEAD)}"
DIST="$REPO/dist"
LDFLAGS="-s -w -X main.version=$VERSION"
rm -rf "$DIST"
mkdir -p "$DIST"

build() { # $1=goos $2=goarch
  local out="$DIST/rca"
  # -buildvcs=false: version comes from ldflags; VCS stamping would fail in
  # containers/worktrees where .git isn't fully visible.
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$out" ./cmd/rca
  tar -C "$DIST" -czf "$DIST/rca_$1_$2.tar.gz" rca
  rm -f "$out"
  echo "built rca_$1_$2.tar.gz"
}

build darwin arm64
build darwin amd64
build linux amd64
build linux arm64

cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum rca_*.tar.gz > checksums.txt
else
  shasum -a 256 rca_*.tar.gz > checksums.txt
fi
ls -la "$DIST"
