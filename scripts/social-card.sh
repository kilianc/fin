#!/usr/bin/env bash
# Render the social preview at its 2400 x 1260 pixels (2x resolution).
# Uses local Chrome and the repository mascot, and the live website’s Google Fonts.
set -euo pipefail
cd "$(dirname "$0")/.."
chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
profile="$(mktemp -d)"
pid=""
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$profile"
}
trap cleanup EXIT

"$chrome" --headless=new --disable-gpu --hide-scrollbars --no-first-run \
  --user-data-dir="$profile/browser" --force-device-scale-factor=2 \
  --window-size=1200,630 --virtual-time-budget=10000 \
  --screenshot="$profile/social-card.png" \
  "file://$PWD/scripts/social-card.html" >"$profile/chrome.log" 2>&1 &
pid=$!
for _ in $(seq 1 40); do
  if [ -s "$profile/social-card.png" ] || ! kill -0 "$pid" 2>/dev/null; then break; fi
  sleep 0.5
done
sleep 0.5
if [ ! -s "$profile/social-card.png" ]; then
  cat "$profile/chrome.log" >&2
  echo "Failed to render the social card" >&2
  exit 1
fi
cp "$profile/social-card.png" docs/social-card.png
echo "docs/social-card.png"
