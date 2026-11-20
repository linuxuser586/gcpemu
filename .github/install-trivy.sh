#!/usr/bin/env bash
# #1165: installs a pinned, checksum-verified Trivy from its GitHub release
# on Linux or macOS (bash 3.2). Never aquasecurity/trivy-action or
# setup-trivy: their tags were hijacked in March 2026 (ADR 0090).
# devtool reads the version below as the one `just licence-check` and
# `just image-scan` require, so it is the only pin.
set -euo pipefail

version=0.76.2

case "$(uname -s)_$(uname -m)" in
  Linux_x86_64) platform=Linux-64bit sha256=bc0fc726e017b8927a62b2d3531d7a84ee01769daf5c99ed866dced54089fdbf ;;
  Linux_aarch64 | Linux_arm64) platform=Linux-ARM64 sha256=ab9d51f86e6a5fb936f605faff6b593ac51bc378fff4d3e8b9d58358b9c367c3 ;;
  Darwin_x86_64) platform=macOS-64bit sha256=e08bb9ea449ecf66c6bfb03cbb8697c503ff33d35fc585c9747e3eaeb7ed4c7b ;;
  Darwin_arm64) platform=macOS-ARM64 sha256=ae7ea358326960406b703c77b2ac490190797d80b306b5b50c2a27221da0a5b8 ;;
  *) echo "unsupported platform $(uname -s) $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive="trivy_${version}_${platform}.tar.gz"
curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 -o "$tmp/$archive" \
  "https://github.com/aquasecurity/trivy/releases/download/v${version}/${archive}"
if command -v sha256sum >/dev/null; then
  echo "$sha256  $tmp/$archive" | sha256sum -c -
else
  echo "$sha256  $tmp/$archive" | shasum -a 256 -c -
fi
tar -xzf "$tmp/$archive" -C "$tmp" trivy
dest=${1:-$HOME/.local/bin}
mkdir -p "$dest"
install -m 0755 "$tmp/trivy" "$dest/trivy"
if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$dest" >>"$GITHUB_PATH"
fi
"$dest/trivy" --version
