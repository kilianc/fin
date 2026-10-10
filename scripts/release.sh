#!/usr/bin/env bash
# Test, build and publish fin as a GitHub Release.
#
#   scripts/release.sh 0.2.0
#
# Tags v0.2.0 on main, pushes the tag, and uploads
# fin-darwin-universal.tar.gz with its SHA256SUMS. The README's install line
# always downloads the latest release's tarball.
#
# The release starts as a draft and is published only once the uploaded files
# download back with the right checksum, so a failed upload never leaves a
# broken latest release. Uploads retry; if they still fail, run the same
# command again: it picks up the existing tag and draft.
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
command -v gh >/dev/null || fail "needs the GitHub CLI (gh)"
if tagged=$(git rev-parse -q --verify "refs/tags/$tag^{commit}"); then
  [[ "$tagged" == "$(git rev-parse HEAD)" ]] || fail "$tag already exists on another commit"
  if draft=$(gh release view "$tag" --json isDraft -q .isDraft 2>/dev/null); then
    [[ "$draft" == "true" ]] || fail "$tag is already released"
  fi
  echo "resuming $tag"
fi

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
git rev-parse -q --verify "refs/tags/$tag" >/dev/null || git tag -a "$tag" -m "fin $version"
git push origin "$tag"
gh release view "$tag" >/dev/null 2>&1 ||
  gh release create "$tag" --draft --verify-tag --title "fin $version" --generate-notes >/dev/null

# Upload, then download what GitHub has and check it, until the two match.
check=$(mktemp -d)
trap 'rm -rf "$check"' EXIT
for try in 1 2 3 4 5 6; do
  rm -rf "${check:?}"/*
  if gh release upload "$tag" "dist/$asset" dist/SHA256SUMS --clobber &&
    gh release download "$tag" -D "$check" &&
    cmp -s "$check/SHA256SUMS" dist/SHA256SUMS &&
    (cd "$check" && shasum -a 256 -c SHA256SUMS >/dev/null); then
    echo "uploaded and verified (try $try)"
    break
  fi
  [[ $try -lt 6 ]] || fail "upload kept failing; run make release VERSION=$version again to resume"
  echo "upload failed; retrying in $((try * 5))s" >&2
  sleep $((try * 5))
done

gh release edit "$tag" --draft=false --latest >/dev/null
gh release view "$tag" --json url -q .url
