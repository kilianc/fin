package fin

import (
	"fmt"
	"strings"

	"github.com/kilianc/fin/internal/ui"
)

// commandSpec describes one command for dispatch and for help.
type commandSpec struct {
	name     string
	usage    string
	summary  string
	detail   string
	examples []string
	group    string
	hidden   bool
	run      func(a *App) command
}

const (
	groupStart = "Get started"
	groupRead  = "Read your data"
	groupLinks = "Manage connections"
	groupMore  = "Experimental"
)

var commands = []commandSpec{
	{
		name: "init", usage: "fin init [--copy]", group: groupStart,
		summary: "Show setup status, the next step, and a prompt for your AI agent",
		detail: `Checks what is done (Plaid keys, environment, connections) and prints a
prompt you can paste into an AI agent that can use your browser. The agent
signs you up for Plaid and its free Trial plan; you only set passwords, accept
terms and paste your secret.

Flags:
  --copy   also copy the agent prompt to the clipboard`,
		examples: []string{"fin init", "fin init --copy"},
		run:      func(a *App) command { return a.cmdInit },
	},
	{
		name: "env", usage: "fin env [sandbox|production]", group: groupStart,
		summary: "Show or switch the Plaid environment",
		detail: `With no argument, shows the current environment and where it came from.
With one, saves it for every later command. The default is sandbox, Plaid's
test environment with fake banks. PLAID_ENV overrides the saved choice for a
single command or shell.`,
		examples: []string{"fin env", "fin env production"},
		run:      func(a *App) command { return a.cmdEnv },
	},
	{
		name: "epoch", usage: "fin epoch [YYYY-MM-DD|none]", group: groupStart,
		summary: "Show or set the first day of the finances fin reports on",
		detail: `The epoch is when your finances, as you want them reported, begin: when a
household started sharing money, say. fin sheet writes transactions and Amazon
items from that day on, fin amazon reads Amazon back to a week before it, and
fin sql can read it from the settings table. Nothing older is deleted; it
stays in the database as history. With no argument, shows it; none clears it.`,
		examples: []string{"fin epoch", "fin epoch 2025-05-01",
			"fin sql \"select sum(amount) from transactions where date >= (select value::date from settings where key = 'epoch')\""},
		run: func(a *App) command { return a.cmdEpoch },
	},
	{
		name: "setup", usage: "fin setup [--from-plaid]", group: groupStart,
		summary: "Save your Plaid keys in the macOS Keychain",
		detail: `Asks for your Plaid client ID and the secret for the current environment
(the secret is hidden as you paste it), checks them with Plaid, and saves both
in the macOS Keychain. Needs a terminal; an agent should ask you to run it.
Find the keys at https://dashboard.plaid.com/developers/keys.

With --from-plaid, nothing is pasted: sign in with Plaid's own CLI
(plaid login, and plaid keys fetch once production is approved), and fin
copies the keys it fetched into the Keychain. No terminal needed.

Flags:
  --from-plaid   import the keys from Plaid's CLI config instead of asking`,
		examples: []string{"fin env production && fin setup", "plaid login && fin setup --from-plaid"},
		run:      func(a *App) command { return a.cmdSetup },
	},
	{
		name: "institutions", usage: "fin institutions <name>", group: groupLinks,
		summary: "Check whether Plaid supports a bank or brokerage",
		detail: `Searches Plaid's institution list. "Bank" means transactions are supported,
"brokerage" means investments are. fin link handles both; use
fin link brokerage only for an institution that is brokerage-only.
Check before linking: every connection uses one of your 10 slots for good.`,
		examples: []string{"fin institutions citi", "fin institutions \"charles schwab\""},
		run:      func(a *App) command { return a.cmdInstitutions },
	},
	{
		name: "link", usage: "fin link [brokerage] [--open] [--yes] [--timeout 30m]", group: groupLinks,
		summary: "Connect a new institution through Plaid Link",
		detail: `Prints a Plaid Link URL, waits while you pick the institution and sign in
in the browser, then saves the connection under a short name such as "chase".
It asks for transactions (24 months of history) and investments, and gets
whichever the institution supports, so one command covers banks, cards and
brokerages. Plaid Link only lists institutions that support transactions;
for the rare brokerage-only institution, use fin link brokerage.

In production each link permanently uses one of 10 slots, so fin asks first.
Without a terminal it stops with CONFIRMATION_REQUIRED unless --yes is given.

Flags:
  --open          open the Link URL in your default browser
  --yes           skip the slot confirmation (agents: only after the user agreed)
  --timeout DUR   how long to wait for Link (default 30m)`,
		examples: []string{"fin link --open", "fin link brokerage --open"},
		run:      func(a *App) command { return a.cmdLink },
	},
	{
		name: "reconnect", usage: "fin reconnect <item> [--add investments|transactions] [--open]", group: groupLinks,
		summary: "Sign in again, or add a product to a connection",
		detail: `Opens Plaid Link in update mode for an existing connection. Use it when
fin items shows "needs reconnect", before consent expires (about once a year),
or with --add to grant transactions or investments to a connection that lacks
them. The connection keeps its name, history and slot.

Flags:
  --add PRODUCT   also grant investments or transactions
  --open          open the Link URL in your default browser
  --timeout DUR   how long to wait for Link (default 30m)`,
		examples: []string{"fin reconnect chase --open", "fin reconnect fidelity --add investments --open"},
		run:      func(a *App) command { return a.cmdReconnect },
	},
	{
		name: "items", usage: "fin items", group: groupLinks,
		summary: "List connections with health, consent expiry and slots used",
		detail: `Health is ok, needs reconnect (run the action shown), error, or unknown.
Consent expiry is when the bank's permission runs out; reconnect before then.`,
		examples: []string{"fin items"},
		run:      func(a *App) command { return a.cmdItems },
	},
	{
		name: "accounts", usage: "fin accounts [--live]", group: groupRead,
		summary: "Balances for every account",
		detail: `Balances come from Plaid's daily refresh.

Flags:
  --live   ask each institution right now (slower, billed per call by Plaid)`,
		examples: []string{"fin accounts", "fin accounts --json | jq '.accounts[].balances.current'"},
		run:      func(a *App) command { return a.cmdAccounts },
	},
	{
		name: "transactions", usage: "fin transactions --since DATE [--until DATE] [--account X]", group: groupRead,
		summary: "Transactions across bank and card accounts, newest first",
		detail: `Syncs new and changed transactions into the local database (see fin sync),
then reads the range from it. In JSON, amounts use Plaid's sign:
positive is money out, negative is money in. The table flips that to read
naturally (negative = spent).

Flags:
  --since DATE    first day, YYYY-MM-DD (required)
  --until DATE    last day, YYYY-MM-DD (default today)
  --account X     an account ID, last four digits, or account name`,
		examples: []string{"fin transactions --since 2026-09-01", "fin transactions --since 2026-01-01 --account 4242 --json"},
		run:      func(a *App) command { return a.cmdTransactions },
	},
	{
		name: "sync", usage: "fin sync", group: groupRead,
		summary: "Pull new and changed transactions into the local database",
		detail: `Asks Plaid only for what changed since the last sync, and writes it to a
DuckDB file in ~/.local/share/fin (one per environment, readable only by you).
The first sync of a connection pulls its full history, up to 24 months.
fin transactions syncs too, so run this before fin sql.`,
		examples: []string{"fin sync", "fin sync && fin sql \"select count(*) from transactions\""},
		run:      func(a *App) command { return a.cmdSync },
	},
	{
		name: "sql", usage: "fin sql \"<query>\"", group: groupRead,
		summary: "Query the local database with DuckDB SQL",
		detail: `Runs one read-only query against the data saved by fin sync. It cannot
change the database or read or write any other file. JSON output has
"columns", "rows" (one object per row) and "synced" (when each connection
was last synced).

Tables:
  transactions  transaction_id, item_id, account_id, date, authorized_date,
                name, merchant_name, amount, iso_currency_code, pending,
                category, category_detailed, payment_channel
  accounts      account_id, item_id, name, official_name, mask, type, subtype,
                current, available, limit, iso_currency_code, updated_at
  items         item_id, item, institution, cursor, status, last_sync
  settings      key, value: the epoch set with fin epoch, under 'epoch'

With a retailer connected (fin amazon), also:
  retailer_items      every item bought: retailer, account, item, product,
                      order_id, line, date, title, quantity, cost (with its
                      share of tax, shipping and discounts), transaction_id
                      (a bank transaction that paid for the order), category,
                      category_detailed, category_source (item, product, none)
  item_categories     retailer, item, category, category_detailed, set_at
  product_categories  retailer, product, category, category_detailed, set_at
  retailer_accounts   retailer, account, last_sync, status, limited_until
and the retailer's own tables (see fin help amazon).

Amounts use Plaid's sign: positive is money out, negative is money in.`,
		examples: []string{
			"fin sql \"select category, sum(amount) from transactions where amount > 0 group by all order by 2 desc\"",
			"fin sql \"select a.name, t.date, t.merchant_name, t.amount from transactions t join accounts a using (account_id) limit 20\"",
		},
		run: func(a *App) command { return a.cmdSQL },
	},
	{
		name: "paths", usage: "fin paths [database]", group: groupRead,
		summary: "Show where fin keeps its state and local database",
		detail: `Lists the state file, the DuckDB file for the current environment, and the
Keychain service. With database, prints only the database path, for scripts.
FIN_CONFIG_DIR and FIN_DATA_DIR move them.`,
		examples: []string{"fin paths", "duckdb \"$(fin paths database)\""},
		run:      func(a *App) command { return a.cmdPaths },
	},
	{
		name: "holdings", usage: "fin holdings [--account X]", group: groupRead,
		summary: "Investment positions with cost basis and tax lots",
		detail: `Tax lots appear when the brokerage reports them; otherwise the list is empty.

Flags:
  --account X     an account ID, last four digits, or account name`,
		examples: []string{"fin holdings", "fin holdings --json"},
		run:      func(a *App) command { return a.cmdHoldings },
	},
	{
		name: "investments", usage: "fin investments --since DATE [--until DATE] [--account X]", group: groupRead,
		summary: "Buys, sells, dividends and fees, newest first",
		detail: `Flags:
  --since DATE    first day, YYYY-MM-DD (required)
  --until DATE    last day, YYYY-MM-DD (default today)
  --account X     an account ID, last four digits, or account name`,
		examples: []string{"fin investments --since 2026-01-01"},
		run:      func(a *App) command { return a.cmdInvestments },
	},
	{
		name: "sheet", usage: "fin sheet [--open] [--new] [--login]", group: groupRead,
		summary: "Keep a Google Sheet with your transactions and balances",
		detail: `Syncs, then writes every stored transaction and account to a spreadsheet
named "fin" in your Google Drive. The first run signs in to Google in your
browser; fin asks only to create files and to edit the ones it created, so it
cannot see anything else in your Drive. The login is kept in the Keychain.

fin owns the Transactions and Accounts tabs and rewrites them on every run.
Add your own tabs for charts and formulas; fin leaves them alone. Amounts use
Plaid's sign: positive is money out, negative is money in.

Flags:
  --open    open the spreadsheet afterwards
  --new     start a new spreadsheet (the old one stays in your Drive)
  --login   sign in to Google again`,
		examples: []string{"fin sheet", "fin sheet --open"},
		run:      func(a *App) command { return a.cmdSheet },
	},
	{
		name: "amazon", usage: "fin amazon [login|sync|categorize|logout|profiles]", group: groupMore,
		summary: "Itemize Amazon orders so their items can be categorized",
		detail: `Experimental. Matches each Amazon charge on your cards to the order it paid
for and the items in it, each with its share of tax and shipping, so the
items in one big charge can each get a category. It reads your amazon.com payments and order pages with
the sign-in your Chrome already has. Amazon offers no API for this: it uses
the website's own endpoints, which Amazon's Conditions of Use do not allow
and which can change or stop working at any time. US amazon.com only.

  fin amazon                     connected accounts, orders, items, matches
  fin amazon login <name>        copy the sign-in from a Chrome profile, then
                                 read every payment and order (minutes the
                                 first time); macOS asks once to allow
                                 "Chrome Safe Storage"
      --profile P                the Chrome profile, by directory or name
      --no-sync                  connect without reading anything yet
  fin amazon sync [name]         read new payments and orders; fin sync does
      --full                     this too. --full re-reads every payment
      --order ID                 read just this order's page again, say after
                                 a return; repeatable, reads no payments
  fin amazon categorize          items without a category
      --all                      every item
      --set ITEM CATEGORY [DETAILED] [--product]
                                 save a category; --product reuses it for
                                 later purchases of the same product. With
                                 no ITEM, reads a JSON list on stdin:
                                 [{"item", "category", "detailed", "product"}]
  fin amazon logout <name>       forget the sign-in and everything read
  fin amazon profiles            Chrome profiles and which are signed in

fin amazon reads back to a week before the epoch set with fin epoch, or
without one, to a week before your oldest bank transaction. It asks Amazon
for one page every few seconds, so the first sync takes a few minutes. If
Amazon says it is getting too many requests, fin stops, keeps what it read,
and leaves Amazon alone for two hours (AMAZON_RATE_LIMITED, with retry_at);
the next sync after that goes on where it stopped.

Categories are Plaid's (FOOD_AND_DRINK, HOME_IMPROVEMENT, …) so they total
up with bank transactions. In fin sql:

  amazon_payments  each charge or refund, with its order IDs
  amazon_orders    order totals: items_subtotal, shipping, discounts, tax,
                   gift_card, paid, refund_total, card_last4
  amazon_items     order_id, line, asin, title, quantity, unit_price, seller,
                   cost (its price plus its share of shipping, discounts
                   and tax, so an order's items add up to it exactly)
  amazon_matches   each payment's match: exact, ambiguous, unmatched, or
                   no_bank_charge (gift cards), with the transaction_id

Amazon's items, with their categories, are also in retailer_items (see fin
help sql), where retailer = 'amazon' and product is the ASIN.

The sign-in is kept encrypted in fin's data directory, with its key in the
Keychain. Your Chrome stays signed in; fin never changes it.`,
		examples: []string{
			"fin epoch 2025-05-01 && fin amazon login home",
			"fin amazon categorize --set 111-1234567-1234567#2 HOME_IMPROVEMENT --product",
			"fin sql \"select date, title, cost, category from retailer_items where retailer = 'amazon' order by date desc\"",
		},
		run: func(a *App) command { return a.cmdAmazon },
	},
	{name: "sandbox-link", hidden: true, run: func(a *App) command { return a.cmdSandboxLink }},
	{name: "sandbox-reset-login", hidden: true, run: func(a *App) command { return a.cmdSandboxResetLogin }},
}

func findCommand(name string) (commandSpec, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return commandSpec{}, false
}

const outputHelp = `  In a terminal, fin prints tables for people. When piped, or when an agent
  runs it, it prints JSON: one object per command with stable field names.
  --json      force JSON (for agents and scripts)
  --table     force the human-readable view
  Errors go to stderr ({"error": {"code", "message"}} in JSON) with exit code 1,
  or 2 for bad arguments. Exit 3 means some connections failed: stdout still
  has everything that worked, plus an "errors" array saying what to run.`

const envHelp = `  PLAID_ENV        sandbox or production; overrides fin env for one command
  FIN_CONFIG_DIR   state directory (default ~/.config/fin)
  FIN_DATA_DIR     local database directory (default $XDG_DATA_HOME/fin,
                   or ~/.local/share/fin)
  FIN_DEBUG        print what Plaid Link reports while waiting`

func (a *App) heading(s string) string {
	if a.human {
		return ui.Accent.Bold(true).Render(s)
	}
	return s
}

func (a *App) printUsage() {
	var b strings.Builder
	if a.human {
		b.WriteString(ui.Brand.Render("fin") + "  " + ui.Muted.Render("bank and brokerage data, on your machine") + "\n\n")
	}
	b.WriteString(wrapText(Tagline, 78) + "\n\n")
	b.WriteString(a.heading("Usage:") + "  fin <command> [flags]\n")
	for _, group := range []string{groupStart, groupRead, groupLinks, groupMore} {
		b.WriteString("\n" + a.heading(group+":") + "\n")
		for _, c := range commands {
			if c.group == group && !c.hidden {
				fmt.Fprintf(&b, "  %-14s %s\n", c.name, c.summary)
			}
		}
	}
	b.WriteString("\n" + a.heading("Output:") + "\n" + outputHelp + "\n\n" + a.heading("Environment:") + "\n" + envHelp + "\n\n")
	b.WriteString("Run fin help <command> or fin <command> --help for flags and examples.\n")
	b.WriteString("New here? Start with fin init.\n")
	ui.Print(a.Stdout, b.String())
}

func (a *App) printCommandHelp(name string) int {
	c, ok := findCommand(name)
	if !ok || c.hidden {
		return a.fail(usageErr("unknown command %q; run fin help", name))
	}
	var b strings.Builder
	b.WriteString(a.heading("fin "+c.name) + " — " + c.summary + "\n\n")
	b.WriteString(a.heading("Usage:") + "\n  " + c.usage + "\n\n")
	b.WriteString(c.detail + "\n")
	if len(c.examples) > 0 {
		b.WriteString("\n" + a.heading("Examples:") + "\n")
		for _, e := range c.examples {
			b.WriteString("  " + e + "\n")
		}
	}
	b.WriteString("\nAdd --json for machine-readable output, --table for the human view.\n")
	ui.Print(a.Stdout, b.String())
	return exitOK
}
