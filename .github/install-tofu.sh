#!/usr/bin/env bash
# Installs a pinned, checksum-verified OpenTofu from its GitHub release on
# Linux, macOS (bash 3.2) or Windows (Git Bash).
# opentofu/setup-opentofu resolves versions through get.opentofu.org's
# api.json, which times out often enough to fail CI.
set -euo pipefail

version=1.13.2

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
  linux_amd64) sha256=ac05d9952662c7db28dd57e93f0a68018837bfdf9342384ec0987977c16a9645 ;;
  linux_arm64) sha256=88e2d9e3f94f97b76ab2faf55ade87578f56aed19c5e45fa139d70e994366d1f ;;
  darwin_amd64) sha256=dc6e4eb1d3ed6550c2ef052596f933dbcd8c806957665ea83ae7537147cb3074 ;;
  darwin_arm64) sha256=0d1cdc85f38992055b48e7fb161304a2ddfe2e5ee0ff68ffec9d0349c8bd1b59 ;;
  windows_amd64) sha256=e74fc2bda0c81ce696b6b7016a4d5c1f557638161239e4101e9ee7d8e0faba59 ;;
  windows_arm64) sha256=4f2fdf19cb01cddc9cf670b41049d438bbf2cb0358cdae8128ef4fb192036a19 ;;
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
