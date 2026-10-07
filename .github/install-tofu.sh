#!/usr/bin/env bash
# Installs a pinned, checksum-verified OpenTofu from its GitHub release.
# opentofu/setup-opentofu resolves versions through get.opentofu.org's
# api.json, which times out often enough to fail CI.
set -euo pipefail

version=1.13.1
declare -A sha256=(
  [amd64]=378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69
  [arm64]=9c1ef375aa1852db0b2888aa921b640c71f8140d4682aa4fec99378a64fa7dc3
)

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive="tofu_${version}_linux_${arch}.tar.gz"
curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 -o "$tmp/$archive" \
  "https://github.com/opentofu/opentofu/releases/download/v${version}/${archive}"
echo "${sha256[$arch]}  $tmp/$archive" | sha256sum -c -
tar -xzf "$tmp/$archive" -C "$tmp" tofu
dest=${1:-$HOME/.local/bin}
mkdir -p "$dest"
install -m 0755 "$tmp/tofu" "$dest/tofu"
if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$dest" >>"$GITHUB_PATH"
fi
"$dest/tofu" version
