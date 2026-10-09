---
name: fin
description: Read the user's own bank, credit card and brokerage data (balances, transactions, holdings, investment activity) with fin, a read-only command-line tool that fetches it from Plaid on their machine. Use it when the user asks about their real money (spending, subscriptions, accounts, net worth, portfolio, cash flow) or wants help setting fin up.
license: MIT
---

# fin

fin is a local command-line tool for the user's own financial data. It reads
balances, transactions, holdings and investment activity from their banks,
cards and brokerages through Plaid, keeps its keys in the macOS Keychain, and
prints JSON when an agent runs it. It cannot move money and never asks Plaid
for account numbers or personal details.

## First, check it's ready

1. Run `fin init --json`. If `fin` isn't installed, install it with
   `go install github.com/kilianc/fin/cmd/fin@latest` (needs Go) and run it
   again.
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
| `fin holdings [--account X]` | Positions with cost basis and tax lots |
| `fin investments --since DATE [--until DATE] [--account X]` | Buys, sells, dividends, fees |
| `fin institutions <name>` | Whether Plaid supports a bank, before using a slot |
| `fin help [command]` | Everything else |

`--account` matches an account ID, its last four digits, or its name. Dates
are `YYYY-MM-DD`.

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
- `fin transactions` re-reads up to 24 months per connection on every call, so
  fetch the widest range you need once and filter it yourself.
- Transfers between the user's own accounts and credit card payments show up
  on both sides. Leave them out of spending totals.

## Rules

- Never run `fin link` in production without the user's explicit go-ahead in
  chat. Each connection permanently uses one of their 10 free Plaid slots.
  Pass `--yes` only after they agree; without a terminal, `fin link` stops
  with `CONFIRMATION_REQUIRED` instead of prompting.
- `fin link` and `fin reconnect` wait until the user finishes in the browser.
  Run them in the background and read the Hosted Link URL from stderr.
- Never ask for, print or store the Plaid secret. Only `fin setup`, run by
  the user in their own terminal, takes it.
- Use fin's numbers. Don't estimate a figure fin can give you, and say which
  accounts and dates a report covers.
