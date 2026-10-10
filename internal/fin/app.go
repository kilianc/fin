// Package fin implements the fin command line: read-only access to bank and
// brokerage data through Plaid, printed as JSON for Claude or as tables.
package fin

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/sheets"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/ui"
)

// SlotsTotal is the number of production Items the Plaid plan allows. fin
// never calls /item/remove, so a slot, once used, stays used.
const SlotsTotal = 10

// Plaid is the subset of the Plaid API fin uses. *plaid.Client implements it;
// tests use a fake.
type Plaid interface {
	VerifyCredentials(ctx context.Context) error
	LinkTokenCreate(ctx context.Context, req plaid.LinkTokenCreateRequest) (*plaid.LinkTokenCreateResponse, error)
	LinkTokenGet(ctx context.Context, linkToken string) (*plaid.LinkTokenGetResponse, error)
	ItemPublicTokenExchange(ctx context.Context, publicToken string) (*plaid.ExchangeResponse, error)
	ItemGet(ctx context.Context, accessToken string) (*plaid.ItemGetResponse, error)
	InstitutionGetByID(ctx context.Context, institutionID string) (*plaid.Institution, error)
	InstitutionsSearch(ctx context.Context, query string, products []string) ([]plaid.InstitutionDetail, error)
	AccountsGet(ctx context.Context, accessToken string) (*plaid.AccountsResponse, error)
	AccountsBalanceGet(ctx context.Context, accessToken string) (*plaid.AccountsResponse, error)
	TransactionsSync(ctx context.Context, accessToken, cursor string, count int) (*plaid.TransactionsSyncResponse, error)
	InvestmentsHoldingsGet(ctx context.Context, accessToken string) (*plaid.HoldingsResponse, error)
	InvestmentsTransactionsGet(ctx context.Context, accessToken, start, end string, offset, count int) (*plaid.InvestmentTransactionsResponse, error)
	SandboxPublicTokenCreate(ctx context.Context, institutionID string, products []string) (string, error)
	SandboxItemResetLogin(ctx context.Context, accessToken string) error
}

// Secrets is where the Plaid credentials and access tokens are kept.
type Secrets interface {
	Get(account string) (string, error)
	Set(account, value string) error
	Delete(account string) error
}

// App holds everything a command needs. main wires the real implementations;
// tests swap in fakes.
type App struct {
	// Version is the release this binary was built from, or "dev".
	Version string
	// Env is resolved by Run from EnvVar (PLAID_ENV), then the environment
	// saved with `fin env`, then sandbox. Tests may set it directly.
	Env       plaid.Env
	EnvVar    string
	StatePath string
	// DataDir holds the DuckDB files, one per environment.
	DataDir string
	// Google is fin's OAuth client for fin sheet, and SheetsBase the Sheets API
	// root (empty means Google's).
	Google     sheets.OAuthClient
	SheetsBase string
	// PlaidCLIConfig is Plaid's own CLI config.json, read by fin setup --from-plaid.
	PlaidCLIConfig string
	// Chrome is where fin amazon login reads Amazon cookies, and AmazonBase
	// the Amazon website (empty means amazon.com).
	Chrome     amazon.Chrome
	AmazonBase string
	// AmazonPause spaces out requests to Amazon.
	AmazonPause time.Duration
	Secrets     Secrets
	NewPlaid    func(env plaid.Env, clientID, secret string) Plaid

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	Now        func() time.Time
	IsTerminal func() bool // stdin
	// StdoutIsTerminal and StderrIsTerminal decide between JSON for agents
	// and styled output for people. Nil means not a terminal.
	StdoutIsTerminal func() bool
	StderrIsTerminal func() bool
	Width            func() int
	ReadLine         func(prompt string) (string, error)
	ReadSecret       func(prompt string) (string, error)
	OpenURL          func(url string) error
	Copy             func(text string) error
	PollInterval     time.Duration
	// Debug prints a summary of each Link poll to stderr (FIN_DEBUG=1).
	Debug bool

	human     bool
	envSource string
	// onScreen is set while a full-screen ui.Flow owns the terminal, so
	// nothing inside it starts a spinner of its own.
	onScreen bool
}

const (
	accountClientID = "plaid.client_id"
	clientUserID    = "fin-local-user"
)

func secretAccount(env plaid.Env) string { return "plaid.secret." + string(env) }

func tokenAccount(env plaid.Env, itemID string) string {
	return string(env) + ".access_token." + itemID
}

type command func(ctx context.Context, args []string) (*result, error)

// Run executes one fin command and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	args, table := extractBool(args, "table")
	args, asJSON := extractBool(args, "json")
	a.human = table || (!asJSON && a.StdoutIsTerminal != nil && a.StdoutIsTerminal())
	if len(args) == 0 {
		a.printUsage()
		return exitUsage
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintf(a.Stdout, "fin %s\n", cmp.Or(a.Version, "dev"))
		return exitOK
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			return a.printCommandHelp(args[1])
		}
		a.printUsage()
		return exitOK
	}
	spec, ok := findCommand(args[0])
	if !ok {
		return a.fail(usageErr("unknown command %q; run fin help", args[0]))
	}
	for _, arg := range args[1:] {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			return a.printCommandHelp(spec.name)
		}
	}
	if a.Env == "" {
		if err := a.resolveEnv(); err != nil {
			return a.fail(err)
		}
	}
	res, err := spec.run(a)(ctx, args[1:])
	if err != nil {
		return a.fail(err)
	}
	return a.emit(res)
}

// showSpinner reports whether a person is watching a full terminal, so
// spinners and live prompts make sense.
func (a *App) showSpinner() bool {
	return a.human && !a.onScreen && a.StderrIsTerminal != nil && a.StderrIsTerminal() && a.IsTerminal != nil && a.IsTerminal()
}

func (a *App) width() int {
	if a.Width == nil {
		return 80
	}
	return a.Width()
}

// resolveEnv picks the Plaid environment: PLAID_ENV if set, else the one
// saved with `fin env`, else sandbox.
func (a *App) resolveEnv() error {
	name, source := a.EnvVar, "PLAID_ENV"
	if name == "" {
		st, err := a.loadState()
		if err != nil {
			return err
		}
		name, source = st.Env, "fin env"
		if name == "" {
			source = "default"
		}
	}
	env, err := plaid.ParseEnv(name)
	if err != nil {
		return usageErr("%v", err)
	}
	a.Env, a.envSource = env, source
	return nil
}

func (a *App) cmdEnv(ctx context.Context, args []string) (*result, error) {
	if len(args) > 1 {
		return nil, usageErr("usage: fin env [sandbox|production]")
	}
	body := map[string]any{"env": a.Env, "source": a.envSource}
	if len(args) == 1 {
		env, err := plaid.ParseEnv(args[0])
		if err != nil || args[0] == "" {
			return nil, usageErr("usage: fin env [sandbox|production]")
		}
		st, err := a.loadState()
		if err != nil {
			return nil, err
		}
		st.Env = string(env)
		if err := a.saveState(st); err != nil {
			return nil, err
		}
		body["saved"] = env
		if a.EnvVar == "" {
			a.Env, a.envSource = env, "fin env"
			body["env"], body["source"] = env, a.envSource
		} else {
			body["warning"] = fmt.Sprintf("PLAID_ENV=%s is set and overrides the saved environment in this shell", a.EnvVar)
		}
	}
	msg := fmt.Sprintf("Environment: %s", ui.Bold.Render(fmt.Sprint(body["env"])))
	switch body["source"] {
	case "PLAID_ENV":
		msg += ui.Muted.Render("  (from PLAID_ENV)")
	case "default":
		msg += ui.Muted.Render("  (default; switch with fin env production)")
	}
	if w, ok := body["warning"].(string); ok {
		msg += "\n" + ui.Line(ui.Warn, w)
	}
	return &result{body: body, message: msg}, nil
}

func (a *App) loadState() (*state.State, error) {
	st, err := state.Load(a.StatePath)
	if err != nil {
		return nil, newErr("STATE_ERROR", "read state: %v", err)
	}
	return st, nil
}

func (a *App) saveState(st *state.State) error {
	if err := st.Save(a.StatePath); err != nil {
		return newErr("STATE_ERROR", "write state: %v", err)
	}
	return nil
}

// plaid builds an API client from the Keychain credentials for the current env.
func (a *App) plaid() (Plaid, error) {
	clientID, err := a.Secrets.Get(accountClientID)
	if err == nil {
		var secret string
		if secret, err = a.Secrets.Get(secretAccount(a.Env)); err == nil {
			return a.NewPlaid(a.Env, clientID, secret), nil
		}
	}
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, newErr("NOT_CONFIGURED",
			"Plaid %s credentials are not in the Keychain; run `fin setup` in a terminal", a.Env)
	}
	return nil, newErr("KEYCHAIN_ERROR", "%v", err)
}

// parseArgs parses flags that may appear before or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr("%s: %v", fs.Name(), err)
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// extractBool removes a global boolean flag from anywhere in args.
func extractBool(args []string, name string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, arg := range args {
		if arg == "--"+name || arg == "-"+name {
			found = true
			continue
		}
		out = append(out, arg)
	}
	return out, found
}

const dateLayout = "2006-01-02"

func parseDate(flagName, value string) (string, error) {
	if _, err := time.Parse(dateLayout, value); err != nil {
		return "", usageErr("--%s must be a date like 2026-01-31, got %q", flagName, value)
	}
	return value, nil
}

// Tagline is the one sentence every new user sees.
const Tagline = "fin is a local, read-only tool for you and your AI agents: your financial data goes from Plaid straight to this machine, your keys stay in your macOS Keychain, and you own all of it end to end."
