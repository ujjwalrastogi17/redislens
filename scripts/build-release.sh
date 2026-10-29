#!/usr/bin/env bash
# Builds release archives for every supported platform into dist/<version>/.
# Usage: scripts/build-release.sh v0.1.0
# Output: redislens_<version>_<os>_<arch>.tar.gz (.zip for Windows) + SHA256SUMS.
set -euo pipefail

VERSION="${1:?usage: $0 <version, e.g. v0.1.0>}"
[[ "$VERSION" == v* ]] || { echo "version must start with v (e.g. v0.1.0)" >&2; exit 2; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist/$VERSION"
TARGETS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64"

cd "$ROOT"
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  echo "warning: building from uncommitted changes" >&2
fi
go vet ./...
go test ./...

rm -rf "$DIST"
mkdir -p "$DIST"
for target in $TARGETS; do
  os="${target%/*}" arch="${target#*/}"
  name="redislens_${VERSION#v}_${os}_${arch}"
  ext=""; [ "$os" = windows ] && ext=".exe"
  mkdir -p "$DIST/$name"
  # CGO_ENABLED=0: a static binary that runs on any Linux, including Alpine.
  # -trimpath: no local paths in the binary. -s -w: drop debug info (smaller).
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$DIST/$name/redislens$ext" ./cmd/redislens
  cp LICENSE "$DIST/$name/"
  [ -f README.md ] && cp README.md "$DIST/$name/"
  if [ "$os" = windows ]; then
    (cd "$DIST" && zip -qr "$name.zip" "$name")
  else
    tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
  fi
  rm -r "${DIST:?}/$name"
  echo "built $name"
done
(cd "$DIST" && shasum -a 256 redislens_* > SHA256SUMS)
ls -lh "$DIST"
