#!/usr/bin/env bash
# End-to-end run of fin against the real Plaid sandbox.
#
# Needs sandbox credentials in the Keychain (PLAID_ENV=sandbox fin setup).
# Uses a throwaway state directory and removes the sandbox access tokens it
# stored from the Keychain on exit. It never touches production.
set -euo pipefail

cd "$(dirname "$0")/.."

export PLAID_ENV=sandbox
FIN_CONFIG_DIR="$(mktemp -d)"
export FIN_CONFIG_DIR
export FIN_DATA_DIR="$FIN_CONFIG_DIR/data"
BIN="$FIN_CONFIG_DIR/fin"

cleanup() {
  if [[ -f "$FIN_CONFIG_DIR/state.json" ]]; then
    for id in $(jq -r '.items[] | select(.env == "sandbox") | .item_id' "$FIN_CONFIG_DIR/state.json"); do
      security delete-generic-password -s fin -a "sandbox.access_token.$id" >/dev/null 2>&1 || true
    done
  fi
  rm -rf "$FIN_CONFIG_DIR"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

go build -o "$BIN" ./cmd/fin
fin() { "$BIN" "$@"; }

step "link a bank and a brokerage Item without the Link UI"
bank=$(fin sandbox-link bank | jq -r '.linked[0].name')
brokerage=$(fin sandbox-link brokerage | jq -r '.linked[0].name')
echo "bank=$bank brokerage=$brokerage"

step "items"
fin items | tee "$FIN_CONFIG_DIR/items.json" | jq -c '.items[] | {name, health, consent_expires_at}'
jq -e '.slots_used == 2 and ([.items[].health] | all(. == "ok"))' "$FIN_CONFIG_DIR/items.json" >/dev/null ||
  fail "expected two healthy Items"

step "accounts"
fin accounts | tee "$FIN_CONFIG_DIR/accounts.json" | jq -c '.accounts[] | {item, name, mask, current: .balances.current}'
jq -e '(.accounts | length) > 0 and (.errors | length) == 0' "$FIN_CONFIG_DIR/accounts.json" >/dev/null ||
  fail "expected accounts and no errors"

step "transactions (sandbox needs a few seconds to pull them)"
since=$(date -v-2y +%F)
for attempt in $(seq 1 20); do
  if fin transactions --since "$since" >"$FIN_CONFIG_DIR/tx.json"; then
    break
  fi
  jq -e '[.errors[].code] | all(. == "TRANSACTIONS_NOT_READY")' "$FIN_CONFIG_DIR/tx.json" >/dev/null ||
    fail "transactions failed: $(jq -c .errors "$FIN_CONFIG_DIR/tx.json")"
  echo "not ready yet (attempt $attempt)"
  sleep 3
done
jq -e '(.transactions | length) > 0 and (.errors | length) == 0' "$FIN_CONFIG_DIR/tx.json" >/dev/null ||
  fail "expected transactions"
echo "$(jq '.transactions | length' "$FIN_CONFIG_DIR/tx.json") transactions"

step "sync is incremental and fin sql reads the local DuckDB file"
fin sync | tee "$FIN_CONFIG_DIR/sync.json" | jq -c '.sync[] | {item, changed, removed}'
jq -e '(.errors | length) == 0' "$FIN_CONFIG_DIR/sync.json" >/dev/null || fail "sync failed"
[[ -f "$FIN_DATA_DIR/sandbox.duckdb" ]] || fail "no sandbox.duckdb"
fin sql "select count(*) as n, round(sum(amount), 2) as total from transactions
         where date between '$since' and current_date" | tee "$FIN_CONFIG_DIR/sql.json" | jq -c '.rows[0]'
jq -e --argjson n "$(jq '.transactions | length' "$FIN_CONFIG_DIR/tx.json")" '.rows[0].n == $n' "$FIN_CONFIG_DIR/sql.json" >/dev/null ||
  fail "fin sql and fin transactions disagree on the count"
if fin sql "delete from transactions" >/dev/null 2>&1; then fail "fin sql accepted a write"; fi

step "holdings"
fin holdings | tee "$FIN_CONFIG_DIR/holdings.json" | jq -c '.holdings[:5][] | {ticker, quantity, value, cost_basis, lots: (.tax_lots | length)}'
jq -e '(.holdings | length) > 0' "$FIN_CONFIG_DIR/holdings.json" >/dev/null || fail "expected holdings"

step "investment transactions"
fin investments --since "$since" >"$FIN_CONFIG_DIR/inv.json" ||
  jq -e '[.errors[].code] | all(. == "PRODUCT_NOT_READY")' "$FIN_CONFIG_DIR/inv.json" >/dev/null ||
  fail "investments failed: $(jq -c .errors "$FIN_CONFIG_DIR/inv.json")"
echo "$(jq '.investment_transactions | length' "$FIN_CONFIG_DIR/inv.json") investment transactions"

step "a broken login is isolated and tells the user what to run"
fin sandbox-reset-login "$bank" >/dev/null
set +e
fin accounts >"$FIN_CONFIG_DIR/broken.json"
code=$?
set -e
[[ $code -eq 3 ]] || fail "expected exit 3, got $code"
jq -e --arg b "$bank" '.errors[0].code == "ITEM_LOGIN_REQUIRED" and .errors[0].action == "run fin reconnect \($b)"' \
  "$FIN_CONFIG_DIR/broken.json" >/dev/null || fail "expected ITEM_LOGIN_REQUIRED with a reconnect action"
jq -e --arg k "$brokerage" '[.accounts[].item] | any(. == $k)' "$FIN_CONFIG_DIR/broken.json" >/dev/null ||
  fail "the healthy Item's accounts are missing"
jq -c '.errors[0] | {item, code, action}' "$FIN_CONFIG_DIR/broken.json"
fin items | jq -e --arg b "$bank" '.items[] | select(.name == $b) | .health == "needs_reconnect"' >/dev/null ||
  fail "items does not show needs_reconnect"

step "Hosted Link token is created and the URL printed"
set +e
fin link bank --timeout 2s >/dev/null 2>"$FIN_CONFIG_DIR/link.err"
code=$?
set -e
grep -q '^https://' "$FIN_CONFIG_DIR/link.err" || fail "no Hosted Link URL: $(cat "$FIN_CONFIG_DIR/link.err")"
grep -q '"LINK_TIMEOUT"' "$FIN_CONFIG_DIR/link.err" || fail "expected LINK_TIMEOUT, exit $code"

printf '\nPASS\n'
