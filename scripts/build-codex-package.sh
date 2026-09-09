#!/usr/bin/env bash
# build-codex-package.sh — build the patched Codex the trusted harness needs.
#
#   scripts/build-codex-package.sh --codex ~/workspace/codex [--out ./dist]
#
# rca's codex-native path needs a Codex carrying the native-state patches:
# without them the native memory and skills backends write to $CODEX_HOME
# instead of the trusted service, and the audited-native handshake fails. Stock
# Codex will not do.
#
# The output is a directory package plus a tarball carrying, alongside the
# binaries, everything Apache-2.0 §4 asks of a modified redistribution: the
# upstream LICENSE, the upstream NOTICE, and a statement of what was changed.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CODEX=""
OUT="$REPO/dist"

while [ $# -gt 0 ]; do
  case "$1" in
    --codex) CODEX="$2"; shift 2 ;;
    --out)   OUT="$2"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

die() { echo "build-codex-package.sh: $*" >&2; exit 1; }

[ -n "$CODEX" ] || die "--codex <path to a codex checkout> is required"
[ -d "$CODEX/codex-rs" ] || die "$CODEX does not look like a codex checkout"

BASELINE="$(grep -v '^#' "$REPO/codex-patches/BASELINE" | tr -d '[:space:]')"
[ -n "$BASELINE" ] || die "codex-patches/BASELINE is empty"

# A package must not be able to claim a baseline it was not built from, and a
# dirty tree must not be able to claim a commit. Both are checked before
# anything is built, because both invalidate the manifest rather than the build.
git -C "$CODEX" merge-base --is-ancestor "$BASELINE" HEAD 2>/dev/null \
  || die "the codex checkout does not contain baseline $BASELINE; rebase the patches or update codex-patches/BASELINE"
[ -z "$(git -C "$CODEX" status --porcelain)" ] \
  || die "the codex checkout has uncommitted changes; commit or stash them so the manifest describes what was built"

HEAD="$(git -C "$CODEX" rev-parse HEAD)"
BASE_FULL="$(git -C "$CODEX" rev-parse "$BASELINE")"
PATCH_COUNT="$(git -C "$CODEX" rev-list --count "$BASE_FULL..HEAD")"
[ "$PATCH_COUNT" -gt 0 ] || die "HEAD is the baseline; there are no patches to build"

echo "codex baseline: $BASE_FULL"
echo "codex HEAD:     $HEAD  ($PATCH_COUNT patches)"

PKG="$OUT/codex-native"
rm -rf "$PKG"
mkdir -p "$OUT"

echo "building (this compiles Codex; expect several minutes)"
( cd "$CODEX/codex-rs" && just assemble-codex-package --package-dir "$PKG" --force )

[ -x "$PKG/bin/codex" ] || die "the package has no bin/codex"

# Apache-2.0 §4(a) and §4(d): carry the licence and the NOTICE.
cp "$CODEX/LICENSE" "$PKG/LICENSE"
[ -f "$CODEX/NOTICE" ] && cp "$CODEX/NOTICE" "$PKG/NOTICE"

# Apache-2.0 §4(b): state that the files were changed, and say how.
{
  echo "# Changes to Codex in this build"
  echo
  echo "This is a modified build of https://github.com/openai/codex, redistributed"
  echo "under the Apache License 2.0. See LICENSE and NOTICE."
  echo
  echo "Upstream baseline: $BASE_FULL"
  echo "Built from:        $HEAD"
  echo
  echo "The modifications route Codex's native memory and skills state through"
  echo "remote-computer-adapter's trusted state service instead of \$CODEX_HOME, so"
  echo "those writes commit with a CAS revision and an audit record. They add no"
  echo "capability to Codex and remove none; without them rca's codex-native"
  echo "handshake fails closed."
  echo
  echo "## Commits applied on top of the baseline"
  echo
  git -C "$CODEX" log --reverse --format='- %h  %s' "$BASE_FULL..HEAD"
} > "$PKG/CHANGES.md"

BINARY_SHA="$(shasum -a 256 "$PKG/bin/codex" 2>/dev/null | awk '{print $1}' \
  || sha256sum "$PKG/bin/codex" | awk '{print $1}')"

cat > "$PKG/rca-codex-manifest.json" <<EOF
{
  "upstream": "https://github.com/openai/codex",
  "upstream_baseline": "$BASE_FULL",
  "built_from": "$HEAD",
  "patch_count": $PATCH_COUNT,
  "binary_sha256": "$BINARY_SHA",
  "built_on": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "license": "Apache-2.0"
}
EOF

# Name the archive the way rca codex-install looks for it, which is Go's
# GOOS_GOARCH — uname says x86_64 where Go says amd64, and an archive named
# after uname is one the installer's download path can never find.
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
TARBALL="$OUT/codex-native_${OS}_${ARCH}.tar.gz"
# COPYFILE_DISABLE keeps macOS tar from emitting an AppleDouble "._name"
# sidecar beside every entry; they are noise in a package meant to be
# extracted anywhere.
COPYFILE_DISABLE=1 tar -C "$OUT" -czf "$TARBALL" codex-native
( cd "$OUT" && { shasum -a 256 "$(basename "$TARBALL")" 2>/dev/null || sha256sum "$(basename "$TARBALL")"; } > "$(basename "$TARBALL").sha256" )

echo
echo "package:  $PKG"
echo "tarball:  $TARBALL"
echo "codex:    $("$PKG/bin/codex" --version 2>/dev/null || echo 'version unavailable')"
echo "binary:   sha256 $BINARY_SHA"
