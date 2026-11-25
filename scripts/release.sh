#!/bin/bash
# Builds the release archives for one version.
#
#   scripts/release.sh v0.1.0
#
# Writes dist/agwmap_<os>_<arch>.tar.gz, each holding kubectl-agwmap, README.md
# and LICENSE. The krew manifest is rendered from .krew.yaml by krew-release-bot
# in the release workflow.
set -euo pipefail

VERSION="${1:?usage: scripts/release.sh <version, for example v0.1.0>}"
PLATFORMS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64"

cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist

for p in $PLATFORMS; do
  os="${p%/*}"; arch="${p#*/}"; bin=kubectl-agwmap
  [ "$os" = windows ] && bin=kubectl-agwmap.exe
  stage="dist/stage_${os}_${arch}"; archive="agwmap_${os}_${arch}.tar.gz"
  mkdir -p "$stage"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$stage/$bin" .
  cp README.md LICENSE "$stage/"
  tar -C "$stage" -czf "dist/$archive" "$bin" README.md LICENSE
  rm -rf "$stage"
done

ls -l dist
