#!/usr/bin/env bash
# Build fin as one universal macOS binary (Apple Silicon and Intel) at bin/fin.
#
#   scripts/build.sh            version "dev"
#   scripts/build.sh 0.2.0      stamps fin --version
#
# Needs an Apple Silicon Mac with the Xcode Command Line Tools: DuckDB links
# with cgo, and clang builds the Intel half with -arch x86_64.
set -euo pipefail

cd "$(dirname "$0")/.."

version="${1:-dev}"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Go 1.26 supports macOS 12 and later; DuckDB's library supports 11. Without
# an explicit target the binary would require the macOS it was built on.
export MACOSX_DEPLOYMENT_TARGET=12.0
export CGO_ENABLED=1
export CGO_CFLAGS="-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
export CGO_CXXFLAGS="$CGO_CFLAGS"
export CGO_LDFLAGS="$CGO_CFLAGS"
ldflags="-s -w -X main.version=$version"

mkdir -p bin
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
GOARCH=arm64 go build -trimpath -ldflags "$ldflags" -o "$tmp/fin-arm64" ./cmd/fin
GOARCH=amd64 CC="clang -arch x86_64" go build -trimpath -ldflags "$ldflags" -o "$tmp/fin-amd64" ./cmd/fin
lipo -create -output bin/fin "$tmp/fin-arm64" "$tmp/fin-amd64"

archs=$(lipo -archs bin/fin)
[[ "$archs" == *arm64* && "$archs" == *x86_64* ]] || fail "bin/fin has only: $archs"
for arch in arm64 x86_64; do
  minos=$(otool -arch "$arch" -l bin/fin | awk '/minos/ {print $2; exit}')
  [[ "$minos" == "$MACOSX_DEPLOYMENT_TARGET" ]] || fail "$arch requires macOS $minos, want $MACOSX_DEPLOYMENT_TARGET"
done
got=$(bin/fin --version)
[[ "$got" == "fin $version" ]] || fail "bin/fin --version printed $got"
echo "bin/fin: $got, universal, macOS $MACOSX_DEPLOYMENT_TARGET+"
