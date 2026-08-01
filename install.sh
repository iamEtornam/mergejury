#!/bin/sh
# Install mergejury from the latest GitHub release.
#
#   curl -fsSL mergejury.etornam.dev/install | sh
#
# Override the install dir with PREFIX, or pin a version with VERSION:
#   PREFIX=~/.local/bin VERSION=v0.1.0 ./install.sh
#
# While the repository is private, release assets need a token with repo
# read access:
#   GITHUB_TOKEN=ghp_... curl -fsSL mergejury.etornam.dev/install | sh
set -eu

REPO=iamEtornam/mergejury
PREFIX="${PREFIX:-/usr/local/bin}"
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"

# auth_curl adds the token header only when one is present, so public
# installs stay anonymous.
auth_curl() {
  if [ -n "$TOKEN" ]; then
    curl -fsSL -H "Authorization: Bearer $TOKEN" "$@"
  else
    curl -fsSL "$@"
  fi
}

die_download() {
  echo "mergejury: could not download $1" >&2
  if [ -z "$TOKEN" ]; then
    echo "  If the repository is private, set GITHUB_TOKEN to a token with repo read access and retry." >&2
  else
    echo "  The token was rejected or the asset is missing for this platform." >&2
  fi
  echo "  Alternative that needs no release asset:" >&2
  echo "    go install github.com/$REPO/cmd/mergejury@latest" >&2
  exit 1
}

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

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

api="https://api.github.com/repos/$REPO/releases"
version="${VERSION:-}"
if [ -z "$version" ]; then
  # Resolve the latest tag without needing jq.
  version=$(auth_curl "$api/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1) || true
fi
if [ -z "$version" ]; then
  die_download "the release list (no published release, or no access)"
fi

archive="mergejury_${version}_${os}_${arch}.tar.gz"

echo "mergejury: downloading $version ($os/$arch)"
# Private repositories do not serve release assets from the browse URL, so
# fetch by asset id through the API when a token is present.
fetch_asset() {
  asset="$1"; out="$2"
  if [ -n "$TOKEN" ]; then
    id=$(auth_curl "$api/tags/$version" 2>/dev/null \
      | tr ',{' '\n\n' \
      | grep -B4 "\"name\": *\"$asset\"" \
      | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1) || true
    [ -n "$id" ] || return 1
    curl -fsSL -H "Authorization: Bearer $TOKEN" \
      -H "Accept: application/octet-stream" \
      "$api/assets/$id" -o "$out"
  else
    curl -fsSL "https://github.com/$REPO/releases/download/$version/$asset" -o "$out"
  fi
}

fetch_asset "$archive" "$tmp/$archive" || die_download "$archive"

# Verify against the release checksums when they are published.
if fetch_asset checksums.txt "$tmp/checksums.txt" 2>/dev/null; then
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
