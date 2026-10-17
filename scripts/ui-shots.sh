#!/usr/bin/env bash
# Renders the fin ui pages to docs/ui/*.png, the images the README and the
# landing page show. The data is the illustrative year scripts/uidemo seeds,
# never anyone's real accounts. Uses the local Chrome in headless mode;
# nothing is installed.
set -euo pipefail

cd "$(dirname "$0")/.."
chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
work="$(mktemp -d)"
mkdir -p docs/ui
go build -o "$work/uidemo" ./scripts/uidemo

"$work/uidemo" -dir "$work/demo" ui >"$work/url.json" &
demo=$!
trap 'kill "$demo" 2>/dev/null || true; rm -rf "$work"' EXIT

# Seeding a year takes about a minute; fin prints the URL once it serves.
for _ in $(seq 1 300); do
  [ -s "$work/url.json" ] && break
  kill -0 "$demo" 2>/dev/null || { echo "uidemo exited" >&2; exit 1; }
  sleep 0.5
done
url="$(sed -n 's/.*"url":"\([^"]*\)".*/\1/p' "$work/url.json")"
[ -n "$url" ] || { echo "no URL from uidemo" >&2; exit 1; }

# Headless Chrome sometimes writes the screenshot and then never exits, so
# each render gets 20 seconds and is stopped once its file appears.
shot() {
  local out="docs/ui/$1.png" path="$2" height="$3"
  rm -f "$out"
  "$chrome" --headless=new --disable-gpu --hide-scrollbars --no-first-run \
    --user-data-dir="$work/chrome-$1" --force-device-scale-factor=2 \
    --window-size=1440,"$height" --virtual-time-budget=5000 \
    --screenshot="$out" "$url$path" >/dev/null 2>&1 &
  local pid=$!
  for _ in $(seq 1 40); do
    if [ -s "$out" ] || ! kill -0 "$pid" 2>/dev/null; then break; fi
    sleep 0.5
  done
  sleep 0.5
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  [ -s "$out" ] || { echo "failed to render $out" >&2; exit 1; }
  echo "$out"
}

# Each page is cut at a natural break: below the cards, the calendar, the
# treemap and the chart.
shot overview "" 1008
shot spending "spending?since=2026-10-01" 842
shot portfolio "portfolio" 1200
shot query "query" 1130
