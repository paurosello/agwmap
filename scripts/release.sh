#!/bin/bash
# Builds the release archives and the krew plugin manifest for one version.
#
#   scripts/release.sh v0.1.0
#
# Writes dist/agwmap_<os>_<arch>.tar.gz (each holds kubectl-agwmap, README.md and
# LICENSE) and plugins/agwmap.yaml, the manifest that krew installs from.
set -euo pipefail

VERSION="${1:?usage: scripts/release.sh <version, for example v0.1.0>}"
REPO="https://github.com/paurosello/agwmap"
PLATFORMS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64"

cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist plugins

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

{
  cat <<EOF
apiVersion: krew.googlecontainertools.github.com/v1alpha2
kind: Plugin
metadata:
  name: agwmap
spec:
  version: ${VERSION}
  homepage: ${REPO}
  shortDescription: Map a cluster's gateway configuration in one HTML page
  description: |
    Reads the Gateway API and agentgateway objects of a cluster and writes one
    self-contained HTML page with the request flow of every route: listener,
    policies at each level, route, backends and upstream targets.
    Captures are anonymized by default and can be exported and rendered again.

      kubectl agwmap                       # current context -> agwmap.html
      kubectl agwmap -n team-a -o a.html
      kubectl agwmap -from snapshot.json
  platforms:
EOF
  for p in $PLATFORMS; do
    os="${p%/*}"; arch="${p#*/}"; bin=kubectl-agwmap
    [ "$os" = windows ] && bin=kubectl-agwmap.exe
    stage="dist/stage_${os}_${arch}"; archive="agwmap_${os}_${arch}.tar.gz"
    mkdir -p "$stage"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$stage/$bin" .
    cp README.md LICENSE "$stage/"
    tar -C "$stage" -czf "dist/$archive" "$bin" README.md LICENSE
    rm -rf "$stage"
    cat <<EOF
  - selector:
      matchLabels:
        os: ${os}
        arch: ${arch}
    uri: ${REPO}/releases/download/${VERSION}/${archive}
    sha256: $(sha "dist/$archive")
    bin: ${bin}
EOF
  done
} > plugins/agwmap.yaml

ls -l dist
