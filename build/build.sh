#!/usr/bin/env bash
# Reproducible arm64 build for PSGNSS_base.
#
# Two runs of this script on the same commit must produce byte-identical
# binaries. That is what makes "the release matches the source" checkable.
set -euo pipefail

cd "$(dirname "$0")/.."

GO_VERSION_REQUIRED="1.24"
BINARY="psgnssd"
OUT="dist/${BINARY}-linux-arm64"

command -v go >/dev/null || { echo "go not found in PATH" >&2; exit 1; }
have="$(go env GOVERSION)"
case "$have" in
  go${GO_VERSION_REQUIRED}*) ;;
  *) echo "warning: building with $have, pinned toolchain is go${GO_VERSION_REQUIRED}.x" >&2 ;;
esac

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"

mkdir -p dist

# Reproducibility levers:
#   CGO_ENABLED=0  no host toolchain leaks in; also makes the binary static
#   -trimpath      strips absolute build paths
#   -buildid=      zeroes the non-deterministic build ID
#   -buildvcs=false  keeps dirty-tree VCS stamps out of the binary
#   GOFLAGS=-mod=readonly  fails rather than silently editing go.mod
export CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOFLAGS=-mod=readonly

go build -trimpath -buildvcs=false \
  -ldflags "-s -w -buildid= \
    -X github.com/psgnss/psgnss-base/internal/version.Version=${VERSION} \
    -X github.com/psgnss/psgnss-base/internal/version.Commit=${COMMIT}" \
  -o "${OUT}" ./cmd/psgnssd

echo "built   : ${OUT}"
echo "version : ${VERSION} (${COMMIT})"
file "${OUT}" 2>/dev/null || true
sha256sum "${OUT}"
