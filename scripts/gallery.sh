#!/usr/bin/env bash
# Renders the cards in docs/examples/index.html to docs/examples/N.png, the
# images the README and the landing page show. Uses the local Chrome in
# headless mode; nothing is installed.
set -euo pipefail

cd "$(dirname "$0")/.."
chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
page="file://$PWD/docs/examples/index.html"
profile="$(mktemp -d)"
trap 'rm -rf "$profile"' EXIT

# Headless Chrome sometimes writes the screenshot and then never exits, so
# each render gets 20 seconds and is stopped once its file appears.
for n in 1 2 3 4; do
  out="docs/examples/$n.png"
  rm -f "$out"
  "$chrome" --headless=new --disable-gpu --hide-scrollbars --no-first-run \
    --user-data-dir="$profile/$n" --force-device-scale-factor=2 \
    --window-size=1200,800 --virtual-time-budget=3000 \
    --screenshot="$out" "$page?card=$n" >/dev/null 2>&1 &
  pid=$!
  for _ in $(seq 1 40); do
    if [ -s "$out" ] || ! kill -0 "$pid" 2>/dev/null; then break; fi
    sleep 0.5
  done
  sleep 0.5
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  [ -s "$out" ] || { echo "failed to render $out" >&2; exit 1; }
  echo "$out"
done
