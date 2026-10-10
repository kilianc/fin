---
name: fin
description: Read the user's own bank, credit card and brokerage data, Amazon orders and Costco receipts and orders with fin, a local read-only command-line tool. Use it when the user asks about their real money (spending, subscriptions, accounts, net worth, portfolio, cash flow), retailer purchases or wants help setting fin up.
license: MIT
---

# fin

fin is a local command-line tool for the user's own financial data. It reads
balances, transactions, holdings and investment activity from their banks,
cards and brokerages through Plaid, keeps its keys in the macOS Keychain, and
prints JSON when an agent runs it. It cannot move money and never asks Plaid
for account numbers or personal details. Connected retailers add Amazon order
items and Costco receipt and online order items, using the user's own sign-in.

## First, check it's ready

1. Run `fin init --json`. If `fin` isn't installed, install the latest
   release with
   `mkdir -p ~/.local/bin && curl -fsSL https://github.com/kilianc/fin/releases/latest/download/fin-darwin-universal.tar.gz | tar -xz -C ~/.local/bin`,
   make sure `~/.local/bin` is on the `PATH`, and run it again.
2. Read `status`; its `next` field says what's left. If setup isn't
   finished, follow the steps in the `prompt` field with the user, using their browser for the clicking and
   leaving them only what needs them: passwords, verification codes, the Plaid
   secret, accepting terms and signing in to their banks.
3. Once `fin items` lists connections, answer their question with the commands
   below.

## Commands

| Command | Returns |
| --- | --- |
| `fin items` | Connections, slots used, consent expiry, health |
| `fin accounts [--live]` | Balances (`--live` asks each bank now and is billed per call; avoid it unless asked) |
| `fin transactions --since DATE [--until DATE] [--account X]` | Transactions, newest first |
| `fin sync` | Pulls new and changed transactions into a local DuckDB file |
| `fin sql "<query>"` | Read-only DuckDB SQL over that file (`fin help sql` lists the tables) |
| `fin sheet` | Syncs and writes everything to a Google Sheet; the first run needs the user to sign in to Google in the browser |
| `fin paths [database]` | Where the state file and the DuckDB file are |
| `fin amazon` | Amazon accounts connected for itemizing orders (experimental), with items left to categorize |
| `fin amazon categorize [--set]` | Items without a category; `--set` saves them (see below) |
| `fin costco` | Costco accounts connected for receipts, online orders and items (experimental) |
| `fin costco categorize [--set]` | Uncategorized Costco items; the same category workflow as Amazon |
| `fin holdings [--account X]` | Positions with cost basis and tax lots |
| `fin investments --since DATE [--until DATE] [--account X]` | Buys, sells, dividends, fees |
| `fin epoch [DATE]` | The first day of the finances the user wants reported |
| `fin institutions <name>` | Whether Plaid supports a bank, before using a slot |
| `fin help [command]` | Everything else |

`--account` matches an account ID, its last four digits, or its name. Dates
are `YYYY-MM-DD`.

## The epoch

`fin epoch` shows the first day of the finances the user wants reported,
such as when a household started sharing money; `fin epoch YYYY-MM-DD` sets
it. Ask before setting it. Default report periods to start no earlier than
the epoch (in SQL: `(select value::date from settings where key = 'epoch')`)
unless the user asks about earlier; older rows are history, not deleted.

## Reading the output

- Without a terminal, fin prints JSON; `--json` forces it. Check the `errors`
  array on every read. Exit code `3` means some connections failed but the
  rest of the data is there: use it and say what's missing.
- An error with an `action` field, such as `"action": "run fin reconnect
  chase"`, needs the user. Tell them what to run; don't retry.
- Transaction amounts use Plaid's sign: positive is money out (a purchase or
  payment), negative is money in (a refund, deposit or paycheck).
- Prefer `authorized_date` over `date` when it's set. Pending transactions can
  still change.
- For anything beyond listing a date range (totals, groupings, merchants,
  months), run `fin sync` once, then answer with `fin sql`. Rows come back as
  objects under `rows`; `synced` says how fresh the data is.
- Transfers between the user's own accounts and credit card payments show up
  on both sides. Leave them out of spending totals.

## Amazon orders (experimental)

If `fin amazon` lists accounts, `fin sync` also reads Amazon payments and
orders, so an Amazon charge can be broken down by item:

- `retailer_items` has every item bought at a connected retailer
  (`retailer = 'amazon'`), with `cost`: its price plus its share of
  shipping, discounts and tax, so an order's items add up to its total. It
  carries the item's `category` and a `transaction_id` that paid for the
  order, when one matched.
- `amazon_matches` says which bank transaction each Amazon payment became
  (`exact`, `ambiguous`, `unmatched`, `no_bank_charge`), with its
  `order_ids`.
- Amazon charges some orders in several payments, one per shipment, and does
  not say which items each payment covered. Report those at the order level;
  don't guess which items a payment paid for.
- After a sync, categorize new items: `fin amazon categorize --json` lists
  them; save with `fin amazon categorize --set --json` and a JSON list on
  stdin: `[{"item": "111-…#1", "category": "HOME_IMPROVEMENT", "detailed":
  "HOME_IMPROVEMENT_HARDWARE", "product": true}]`. Use Plaid's
  categories (the list comes back in `categories`); set `product` for
  things bought repeatedly. Ask the user when an item's purpose is unclear.
- `AMAZON_RATE_LIMITED` means Amazon refused requests. fin won't ask again
  before `retry_at`; don't retry sooner, and answer from what is stored.
- `fin amazon login <name>` needs the user: macOS asks them to allow
  "Chrome Safe Storage". Never run it, or `fin amazon logout`, without their
  go-ahead in chat. It reads back to a week before the epoch (see below).

## Costco receipts and online orders (experimental)

If `fin costco` lists accounts, `fin sync` also reads Costco warehouse, gas
station and car wash receipts and costco.com orders. `fin costco sync
[name]` reads only Costco, back to a week before the epoch or oldest bank
transaction (else two years). Each saved window advances the initial read;
an interrupted sync resumes there. Later syncs reread the last 60 days for
returns. fin sees only the signed-in membership: purchases family make with
a Costco Shop Card the user gave them are not there (buying the card is an
online order).

- `retailer_items` includes Costco with `retailer = 'costco'`, `product` =
  Costco's item number, `order_id` = receipt barcode or order number, `line`
  = position. Item handles are `<account>/<barcode or order>#<line>`.
  Receipt or order items always sum to its total. On a receipt, `cost` is
  the amount plus its share of the tax Costco charged on taxed lines
  (`tax_flag` Y) and their discounts; untaxed lines get none. Keep negative
  discount lines (`discount_for` names the line) as their own items; their
  `product` is the discounted item's number, so a product category covers
  them. On an online order Costco doesn't say which item a coupon, fee or
  the tax belongs to, so they are shared by amount.
- `costco_receipts` and `costco_orders` hold totals and raw JSON; a row with
  `error` set could not be read and has no items (report it, don't
  reconstruct it). `costco_items`/`costco_order_items` hold lines,
  `costco_tenders`/`costco_order_payments` each payment. `costco_matches`
  matches each payment (`source` warehouse or online): `exact`,
  `ambiguous`, `unmatched` or `no_bank_charge` (cash, shop card, rewards,
  coupons), on amount, Costco merchant/name, and the bank's authorized date
  (else date) within 3 days of a receipt or 10 of an order (costco.com
  charges when it ships). A receipt's `card_last4` is the card number,
  which often differs from the account's `mask`, so it is not used. Never
  turn an ambiguous or unmatched payment into a guessed bank match.
- Use `fin costco categorize --json`, then `fin costco categorize --set
  --json` with the same JSON list and Plaid categories described above.
  `product: true` sets a default by item number. Categories survive resyncs
  and reparsing. `fin sheet` includes a Costco items tab.
- Avoid counting a receipt's items and its matched bank charge twice.
- `COSTCO_RATE_LIMITED` includes `retry_at`; answer from stored data and do
  not retry sooner. `COSTCO_SIGNIN_EXPIRED` needs the user to sign in to
  costco.com in Chrome, then run `fin costco login <name>` again.
- Ask in chat before connecting or logging out. Login reads the selected
  Chrome profile once; macOS may ask for Chrome Safe Storage if its MSAL
  cache is encrypted. Tokens are sealed locally with a Keychain key. This
  uses undocumented US costco.com endpoints, may be against its terms, and
  can break or be blocked. Nothing runs until the user connects an account.

## Rules

- Never run `fin link` in production without the user's explicit go-ahead in
  chat. Each connection permanently uses one of their 10 free Plaid slots.
  Pass `--yes` only after they agree; without a terminal, `fin link` stops
  with `CONFIRMATION_REQUIRED` instead of prompting.
- `fin link` and `fin reconnect` wait until the user finishes in the browser.
  Run them in the background and read the Hosted Link URL from stderr. So
  does `fin sheet` the first time, while the user signs in to Google; the
  sign-in URL is on stderr.
- Never ask for, print or store the Plaid secret. Only `fin setup`, run by
  the user in their own terminal, takes it. If they use Plaid's own CLI,
  `plaid login` followed by `fin setup --from-plaid` imports the keys with
  nothing pasted.
- Use fin's numbers. Don't estimate a figure fin can give you, and say which
  accounts and dates a report covers.
