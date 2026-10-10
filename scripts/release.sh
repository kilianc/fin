#!/usr/bin/env bash
# Test, build and publish fin as a GitHub Release.
#
#   scripts/release.sh 0.2.0
#
# Tags v0.2.0 on main, pushes the tag, and uploads
# fin-darwin-universal.tar.gz with its SHA256SUMS. The README's install line
# always downloads the latest release's tarball.
set -euo pipefail

cd "$(dirname "$0")/.."

version="${1:-}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "usage: make release VERSION=X.Y.Z" >&2; exit 2; }
tag="v$version"
asset="fin-darwin-universal.tar.gz"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

[[ -z "$(git status --porcelain)" ]] || fail "the working tree is not clean"
[[ "$(git branch --show-current)" == "main" ]] || fail "release from main"
! git rev-parse -q --verify "refs/tags/$tag" >/dev/null || fail "$tag already exists"
command -v gh >/dev/null || fail "needs the GitHub CLI (gh)"

step "test"
make test

step "build $tag"
scripts/build.sh "$version"
rm -rf dist
mkdir -p dist
tar -czf "dist/$asset" -C bin fin
(cd dist && shasum -a 256 "$asset" >SHA256SUMS)
cat dist/SHA256SUMS

step "publish $tag"
git tag -a "$tag" -m "fin $version"
git push origin "$tag"
gh release create "$tag" "dist/$asset" dist/SHA256SUMS --title "fin $version" --generate-notes
