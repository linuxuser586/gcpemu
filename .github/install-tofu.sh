#!/usr/bin/env bash
# Installs a pinned, checksum-verified OpenTofu from its GitHub release on
# Linux, macOS (bash 3.2) or Windows (Git Bash).
# opentofu/setup-opentofu resolves versions through get.opentofu.org's
# api.json, which times out often enough to fail CI.
set -euo pipefail

version=1.13.1

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW* | MSYS* | CYGWIN*) os=windows ;;
  *) echo "unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

case "${os}_${arch}" in
  linux_amd64) sha256=378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69 ;;
  linux_arm64) sha256=9c1ef375aa1852db0b2888aa921b640c71f8140d4682aa4fec99378a64fa7dc3 ;;
  darwin_amd64) sha256=a73720443ba38712d7d96dc1e857add02c15a790919c653ad07492e9952f8c27 ;;
  darwin_arm64) sha256=be78f659f04ef06a9dbd9b3934d46af95d787a3aa38396d459dea395261816a9 ;;
  windows_amd64) sha256=6825ae311e9a15747ce2ede7b7e97c8bb744a4d271fa8113259eeeee3d09438c ;;
  windows_arm64) sha256=cc189b6f7ecba6c80924ddff72b4d086d5f727a0c31180421cfb821a3e5c7587 ;;
esac
exe=
[ "$os" = windows ] && exe=.exe

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive="tofu_${version}_${os}_${arch}.tar.gz"
curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 -o "$tmp/$archive" \
  "https://github.com/opentofu/opentofu/releases/download/v${version}/${archive}"
if command -v sha256sum >/dev/null; then
  echo "$sha256  $tmp/$archive" | sha256sum -c -
else
  echo "$sha256  $tmp/$archive" | shasum -a 256 -c -
fi
tar -xzf "$tmp/$archive" -C "$tmp" "tofu$exe"
dest=${1:-$HOME/.local/bin}
mkdir -p "$dest"
install -m 0755 "$tmp/tofu$exe" "$dest/tofu$exe"
if [ -n "${GITHUB_PATH:-}" ]; then
  # The runner reads GITHUB_PATH as a native path.
  if command -v cygpath >/dev/null; then
    cygpath -w "$dest" >>"$GITHUB_PATH"
  else
    echo "$dest" >>"$GITHUB_PATH"
  fi
fi
"$dest/tofu$exe" version
