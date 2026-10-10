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
	for _, group := range []string{groupStart, groupRead, groupLinks} {
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
