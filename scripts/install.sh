#!/bin/sh
# install.sh — install rca.
#
#   curl -fsSL https://raw.githubusercontent.com/hoveychen/remote-computer-adapter/main/scripts/install.sh | sh
#
# Environment:
#   RCA_INSTALL_DIR   where to install (default: the first writable of
#                     /usr/local/bin, ~/.local/bin)
#   RCA_RELEASE_BASE  where to download from (default: the latest release)
#
# POSIX sh on purpose: this runs on whatever the remote host has.
set -eu

RELEASE_BASE="${RCA_RELEASE_BASE:-https://github.com/hoveychen/remote-computer-adapter/releases/latest/download}"

die() { echo "install.sh: $*" >&2; exit 1; }

# Map uname onto a published archive. An unrecognised platform stops here,
# where the values that confused it are visible, rather than downloading
# something the host cannot execute.
detect_platform() {
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) die "unsupported OS $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  echo "${os}_${arch}"
}

# Pick an install directory we can actually write to, so the failure is "no
# writable directory" rather than a permission error halfway through.
choose_dir() {
  if [ -n "${RCA_INSTALL_DIR:-}" ]; then
    mkdir -p "$RCA_INSTALL_DIR" || die "cannot create $RCA_INSTALL_DIR"
    [ -w "$RCA_INSTALL_DIR" ] || die "$RCA_INSTALL_DIR is not writable"
    echo "$RCA_INSTALL_DIR"
    return
  fi
  for dir in /usr/local/bin "$HOME/.local/bin"; do
    if [ -d "$dir" ] && [ -w "$dir" ]; then echo "$dir"; return; fi
  done
  mkdir -p "$HOME/.local/bin" || die "cannot create $HOME/.local/bin"
  echo "$HOME/.local/bin"
}

fetch() { # url dest
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$2" "$1" || die "download failed: $1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1" || die "download failed: $1"
  else
    die "need curl or wget"
  fi
}

sha256_of() { # file
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else die "need sha256sum or shasum"
  fi
}

platform="$(detect_platform)"
archive="rca_${platform}.tar.gz"
dir="$(choose_dir)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "rca: downloading $archive"
fetch "$RELEASE_BASE/$archive" "$tmp/$archive"
fetch "$RELEASE_BASE/checksums.txt" "$tmp/checksums.txt"

want="$(awk -v want="$archive" '{ n = $2; sub(/^\*/, "", n); if (n == want) print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $archive"
got="$(sha256_of "$tmp/$archive")"
# Verify before extracting: a tampered archive should never be unpacked, not
# unpacked and then regretted.
[ "$got" = "$want" ] || die "checksum mismatch for $archive: got $got, want $want"

tar -xzf "$tmp/$archive" -C "$tmp" || die "cannot extract $archive"
[ -f "$tmp/rca" ] || die "archive contains no rca binary"
chmod 755 "$tmp/rca"

# Move into place rather than overwrite: a binary that is currently running
# must not be modified underneath it.
mv -f "$tmp/rca" "$dir/rca.incoming" && mv -f "$dir/rca.incoming" "$dir/rca" \
  || die "cannot install into $dir"

echo "rca: installed $("$dir/rca" version) at $dir/rca"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "rca: note — $dir is not on your PATH" ;;
esac
