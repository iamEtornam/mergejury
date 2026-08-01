#!/bin/sh
# Install mergejury from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/iamEtornam/mergejury/main/install.sh | sh
#
# Override the install dir with PREFIX, or pin a version with VERSION:
#   PREFIX=~/.local/bin VERSION=v0.1.0 ./install.sh
set -eu

REPO=iamEtornam/mergejury
PREFIX="${PREFIX:-/usr/local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "mergejury: unsupported architecture $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "mergejury: unsupported OS $os (on Windows, download the zip from the releases page)" >&2; exit 1 ;;
esac

version="${VERSION:-}"
if [ -z "$version" ]; then
  # Resolve the latest tag without needing jq.
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi
if [ -z "$version" ]; then
  echo "mergejury: could not determine the latest version. Is there a published release?" >&2
  echo "  If the repository is private, download the archive manually or use: go install github.com/$REPO/cmd/mergejury@latest" >&2
  exit 1
fi

archive="mergejury_${version}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$version/$archive"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "mergejury: downloading $version ($os/$arch)"
curl -fsSL "$url" -o "$tmp/$archive"

# Verify against the release checksums when they are published.
if curl -fsSL "https://github.com/$REPO/releases/download/$version/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  expected=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
    else
      actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
    fi
    if [ "$expected" != "$actual" ]; then
      echo "mergejury: checksum mismatch, refusing to install" >&2
      echo "  expected $expected" >&2
      echo "  actual   $actual" >&2
      exit 1
    fi
    echo "mergejury: checksum verified"
  fi
fi

tar -xzf "$tmp/$archive" -C "$tmp"

if [ -w "$PREFIX" ]; then
  install -m 0755 "$tmp/mergejury" "$PREFIX/mergejury"
else
  echo "mergejury: $PREFIX is not writable, using sudo"
  sudo install -m 0755 "$tmp/mergejury" "$PREFIX/mergejury"
fi

echo "mergejury: installed to $PREFIX/mergejury"
"$PREFIX/mergejury" --version
echo
echo "Next: mergejury adapters check"
