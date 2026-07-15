#!/bin/sh
# Install the latest (or a pinned) blueprint release binary from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/hoophq/blueprint/main/install.sh | sh
#
# Environment overrides:
#   BLUEPRINT_VERSION      install a specific version (e.g. v0.1.0); default latest
#   BLUEPRINT_INSTALL_DIR  target directory; default /usr/local/bin when writable,
#                          otherwise ~/.local/bin
#
# Downloads the same tar.gz archives the Homebrew formula uses and verifies
# them against the release's checksums.txt. macOS and Linux only — on Windows
# download the zip from https://github.com/hoophq/blueprint/releases
set -eu

REPO="hoophq/blueprint"
BIN="blueprint"

err() {
  echo "install.sh: $*" >&2
  exit 1
}

os=$(uname -s)
case "$os" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) err "unsupported OS '$os' — on Windows download the zip from https://github.com/$REPO/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) err "unsupported architecture '$arch'" ;;
esac

version="${BLUEPRINT_VERSION:-}"
if [ -z "$version" ]; then
  # Resolve the latest tag from the release redirect: no API rate limits, no
  # JSON parsing dependency.
  version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
    err "could not resolve the latest release of $REPO"
  version="${version##*/}"
  case "$version" in
    v[0-9]*) ;;
    *) err "could not parse the latest release tag (got '$version')" ;;
  esac
fi
# Accept both "0.1.0" and "v0.1.0" in BLUEPRINT_VERSION.
case "$version" in v*) ;; *) version="v$version" ;; esac
semver="${version#v}"

archive="${BIN}_${semver}_${os}_${arch}.tar.gz"
base_url="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "downloading $base_url/$archive"
curl -fsSL -o "$tmp/$archive" "$base_url/$archive" ||
  err "download failed — does release $version ship $archive?"

# Verify against checksums.txt. Releases older than the checksum file get a
# warning instead of a hard failure.
if curl -fsSL -o "$tmp/checksums.txt" "$base_url/checksums.txt" 2>/dev/null; then
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
  else
    err "found checksums.txt but neither sha256sum nor shasum is available to verify"
  fi
  expected=$(grep " ${archive}\$" "$tmp/checksums.txt" | cut -d' ' -f1)
  [ -n "$expected" ] || err "checksums.txt has no entry for $archive"
  [ "$actual" = "$expected" ] ||
    err "checksum mismatch for $archive (expected $expected, got $actual)"
  echo "checksum verified"
else
  echo "warning: release $version has no checksums.txt; skipping verification" >&2
fi

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"

install_dir="${BLUEPRINT_INSTALL_DIR:-}"
if [ -z "$install_dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="$HOME/.local/bin"
  fi
fi

mkdir -p "$install_dir"
install -m 0755 "$tmp/$BIN" "$install_dir/$BIN"
echo "installed $BIN $version to $install_dir/$BIN"

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "note: $install_dir is not on your PATH — add it, e.g.: export PATH=\"$install_dir:\$PATH\"" >&2 ;;
esac
