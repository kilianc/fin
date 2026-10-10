package fin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/ui"
)

// linkProducts are the only products fin ever requests, all read-only; Auth
// and Identity are deliberately absent. Each kind requires one product, which
// decides which institutions Link lists, and also asks for the other wherever
// the institution supports it. So a brokerage linked as "bank" still gets
// investments, and nobody has to relink after guessing wrong.
var linkProducts = map[string]linkKind{
	"bank":      {required: "transactions", ifSupported: "investments"},
	"brokerage": {required: "investments", ifSupported: "transactions"},
}

type linkKind struct {
	required    string
	ifSupported string
}

// transactionsHistoryDays asks for the full 24 months Plaid can provide. It
// can only be set when an Item is first linked.
const transactionsHistoryDays = 730

const sandboxInstitution = "ins_109508" // First Platypus Bank

func (a *App) cmdSetup(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fromPlaid := fs.Bool("from-plaid", false, "import the keys Plaid's own CLI fetched with plaid login")
	if pos, err := parseArgs(fs, args); err != nil {
		return nil, err
	} else if len(pos) > 0 {
		return nil, usageErr("usage: fin setup [--from-plaid]")
	}
	if !*fromPlaid && a.showSpinner() {
		return a.setupScreen(ctx)
	}
	var clientID, secret string
	var err error
	if *fromPlaid {
		clientID, secret, err = a.plaidCLIKeys(a.Env)
	} else {
		clientID, secret, err = a.promptKeys()
	}
	if err != nil {
		return nil, err
	}
	if err := a.saveKeys(ctx, a.Env, clientID, secret); err != nil {
		return nil, err
	}
	body := map[string]any{"env": a.Env, "stored": []string{accountClientID, secretAccount(a.Env)}, "verified": true}
	msg := ui.Line(ui.Good, fmt.Sprintf("Plaid accepted your %s keys. They are saved in your macOS Keychain, nowhere else.", a.Env))
	if *fromPlaid {
		body["imported_from"] = a.PlaidCLIConfig
		note := fmt.Sprintf("Plaid's CLI keeps its own copy in %s; run plaid logout if you no longer need it.", a.PlaidCLIConfig)
		body["note"] = note
		msg = ui.Line(ui.Good, fmt.Sprintf("Imported your %s keys from Plaid's CLI. Plaid accepted them, and they are saved in your macOS Keychain.", a.Env)) +
			"\n" + ui.Muted.Render(note)
	}
	return &result{body: body, message: msg}, nil
}

// saveKeys checks the keys with Plaid, then stores them in the Keychain.
func (a *App) saveKeys(ctx context.Context, env plaid.Env, clientID, secret string) error {
	if err := a.NewPlaid(env, clientID, secret).VerifyCredentials(ctx); err != nil {
		return err
	}
	if err := a.Secrets.Set(accountClientID, clientID); err != nil {
		return newErr("KEYCHAIN_ERROR", "%v", err)
	}
	if err := a.Secrets.Set(secretAccount(env), secret); err != nil {
		return newErr("KEYCHAIN_ERROR", "%v", err)
	}
	return nil
}

const plaidKeysURL = "https://dashboard.plaid.com/developers/keys"

// setupScreen is fin setup as a full-screen form, for a person at a terminal.
func (a *App) setupScreen(ctx context.Context) (*result, error) {
	stored, err := a.Secrets.Get(accountClientID)
	if err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return nil, newErr("KEYCHAIN_ERROR", "%v", err)
	}
	opts := ui.SetupOptions{
		Env:      string(a.Env),
		ClientID: stored,
		KeysURL:  plaidKeysURL,
		Verify: func(ctx context.Context, env, clientID, secret string) error {
			if err := a.NewPlaid(plaid.Env(env), clientID, secret).VerifyCredentials(ctx); err != nil {
				return errors.New(asCLIError(err).Message)
			}
			return nil
		},
		Save: func(env, clientID, secret string) error {
			if err := a.Secrets.Set(accountClientID, clientID); err != nil {
				return err
			}
			return a.Secrets.Set(secretAccount(plaid.Env(env)), secret)
		},
		Open: a.OpenURL,
		Next: "fin link --open",
	}
	// PLAID_ENV pins the environment for this shell, so it can't be switched here.
	if a.EnvVar == "" {
		opts.Envs = []string{string(plaid.Sandbox), string(plaid.Production)}
		opts.SwitchEnv = func(env string) error {
			st, err := a.loadState()
			if err != nil {
				return err
			}
			st.Env = env
			return a.saveState(st)
		}
	}
	if _, err := os.Stat(a.PlaidCLIConfig); err == nil {
		opts.Import = func(env string) (string, string, error) {
			id, secret, err := a.plaidCLIKeys(plaid.Env(env))
			if err != nil {
				return "", "", errors.New(asCLIError(err).Message)
			}
			return id, secret, nil
		}
	}
	res, err := ui.Setup(ctx, a.Stdin, a.Stderr, opts)
	if errors.Is(err, ui.ErrCancelled) {
		return nil, newErr("CANCELLED", "setup cancelled; nothing was saved")
	}
	if err != nil {
		return nil, err
	}
	a.Env = plaid.Env(res.Env)
	body := map[string]any{"env": a.Env, "stored": []string{accountClientID, secretAccount(a.Env)}, "verified": true, "imported": res.Imported}
	msg := ui.Line(ui.Good, fmt.Sprintf("Your %s keys are saved in your macOS Keychain. Next: fin link --open", a.Env))
	return &result{body: body, message: msg}, nil
}

// promptKeys asks for the client ID and secret in the terminal.
func (a *App) promptKeys() (string, string, error) {
	if !a.IsTerminal() {
		return "", "", newErr("NOT_A_TERMINAL", "fin setup reads secrets interactively; run it in a terminal, or use fin setup --from-plaid after plaid login")
	}
	if a.human {
		ui.Print(a.Stdout, ui.Mascot(a.width(), a.Stdout)+"\n\n")
	}
	clientID, err := a.Secrets.Get(accountClientID)
	if err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return "", "", newErr("KEYCHAIN_ERROR", "%v", err)
	}
	prompt := "Plaid client ID: "
	if clientID != "" {
		prompt = "Plaid client ID [Enter keeps the stored one]: "
	}
	line, err := a.ReadLine(prompt)
	if err != nil {
		return "", "", err
	}
	if line = strings.TrimSpace(line); line != "" {
		clientID = line
	}
	if clientID == "" {
		return "", "", usageErr("a client ID is required")
	}
	secret, err := a.ReadSecret(fmt.Sprintf("Plaid %s secret (input hidden): ", a.Env))
	if err != nil {
		return "", "", err
	}
	if secret = strings.TrimSpace(secret); secret == "" {
		return "", "", usageErr("a secret is required")
	}
	return clientID, secret, nil
}

// plaidCLIConfig is the part of Plaid's CLI config.json that fin reads. The
// CLI writes it after plaid login (Sandbox) and plaid keys fetch (Production).
type plaidCLIConfig struct {
	ClientID     string `json:"client_id"`
	Environments map[string]struct {
		Secret string `json:"secret"`
	} `json:"environments"`
}

// plaidCLIKeys reads the client ID and the current environment's secret from
// Plaid's CLI, so signing in through the Plaid Dashboard replaces pasting keys.
func (a *App) plaidCLIKeys(env plaid.Env) (string, string, error) {
	path := a.PlaidCLIConfig
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", newErr("PLAID_CLI_NOT_FOUND",
			"no Plaid CLI config at %s; install it with brew install plaid/plaid-cli/plaid, then run plaid login", path)
	}
	if err != nil {
		return "", "", newErr("PLAID_CLI_ERROR", "read %s: %v", path, err)
	}
	var cfg plaidCLIConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", newErr("PLAID_CLI_ERROR", "%s is not the config fin expects: %v", path, err)
	}
	if cfg.ClientID == "" {
		return "", "", newErr("PLAID_CLI_NOT_LOGGED_IN", "Plaid's CLI has no client ID yet; run plaid login first")
	}
	secret := cfg.Environments[string(env)].Secret
	if secret == "" {
		hint := "run plaid login first"
		if env == plaid.Production {
			hint = "production keys arrive once your Trial plan is approved; then run plaid keys fetch"
		}
		return "", "", newErr("PLAID_CLI_NO_SECRET", "Plaid's CLI has no %s secret; %s", env, hint)
	}
	return cfg.ClientID, secret, nil
}

type linkBody struct {
	Env        string     `json:"env"`
	Linked     []itemView `json:"linked"`
	SlotsUsed  int        `json:"slots_used"`
	SlotsTotal int        `json:"slots_total"`
}

func (a *App) cmdLink(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the production slot confirmation")
	open := fs.Bool("open", false, "open the Hosted Link URL in the default browser")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to wait for Link to finish")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	kind := "bank"
	if len(pos) == 1 {
		kind = pos[0]
	}
	if _, ok := linkProducts[kind]; !ok || len(pos) > 1 {
		return nil, usageErr("usage: fin link [brokerage] [--open] [--timeout 30m] [--yes]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	used := len(st.ForEnv(string(a.Env)))
	if a.Env == plaid.Production {
		if used >= SlotsTotal {
			return nil, newErr("NO_SLOTS", "all %d production Item slots are used", SlotsTotal)
		}
		if !*yes {
			msg := fmt.Sprintf("Linking in production uses Item slot %d of %d, permanently (%d used).", used+1, SlotsTotal, used)
			if err := a.confirm(msg); err != nil {
				return nil, err
			}
		}
	}
	api, err := a.plaid()
	if err != nil {
		return nil, err
	}
	req := a.linkTokenRequest()
	lk := linkProducts[kind]
	req.Products = []string{lk.required}
	req.RequiredIfSupportedProducts = []string{lk.ifSupported}
	req.Transactions = &plaid.LinkTransactions{DaysRequested: transactionsHistoryDays}
	sess, err := a.runHostedLink(ctx, api, req, *open, *timeout)
	if err != nil {
		return nil, err
	}
	tokens := publicTokens(sess)
	if len(tokens) == 0 {
		return nil, linkExitErr(sess)
	}
	body := linkBody{Env: string(a.Env), Linked: []itemView{}, SlotsTotal: SlotsTotal}
	for i, pub := range tokens {
		var inst *plaid.Institution
		if sess.Results != nil && i < len(sess.Results.ItemAddResults) {
			inst = sess.Results.ItemAddResults[i].Institution
		}
		v, err := a.addItem(ctx, api, st, kind, pub, inst)
		if err != nil {
			return nil, err
		}
		body.Linked = append(body.Linked, v)
	}
	body.SlotsUsed = len(st.ForEnv(string(a.Env)))
	return &result{body: body, message: linkedMessage("Linked", body.Linked)}, nil
}

func (a *App) cmdReconnect(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("reconnect", flag.ContinueOnError)
	open := fs.Bool("open", false, "open the Hosted Link URL in the default browser")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to wait for Link to finish")
	add := fs.String("add", "", "also grant a product: investments or transactions")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 || (*add != "" && !readOnlyProducts[*add]) {
		return nil, usageErr("usage: fin reconnect <item> [--add investments|transactions] [--open] [--timeout 30m]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	it, err := a.findItem(st, pos[0])
	if err != nil {
		return nil, err
	}
	token, err := a.Secrets.Get(tokenAccount(a.Env, it.ItemID))
	if err != nil {
		return nil, toItemErrorAsCLI(it, err)
	}
	api, err := a.plaid()
	if err != nil {
		return nil, err
	}
	if *add != "" && it.HasProduct(*add) {
		return nil, usageErr("%s already has %s", it.Name, *add)
	}
	// Update mode: pass the access token and no products. New products go in
	// additional_consented_products, never products.
	req := a.linkTokenRequest()
	req.AccessToken = token
	if *add != "" {
		req.AdditionalConsentedProducts = []string{*add}
	}
	sess, err := a.runHostedLink(ctx, api, req, *open, *timeout)
	if err != nil {
		return nil, err
	}
	if exit := sessionExit(sess); exit != nil && exit.Error != nil {
		return nil, linkExitErr(sess)
	}
	ig, err := api.ItemGet(ctx, token)
	if err != nil {
		return nil, err
	}
	v := newItemView(it)
	v.applyItemGet(it, ig)
	if v.Health != "ok" {
		return nil, &CLIError{
			Code:    "RECONNECT_INCOMPLETE",
			Message: fmt.Sprintf("%s still reports %s after Link finished", it.Name, v.Error.Code),
			Details: map[string]any{"item": v},
			exit:    exitError,
		}
	}
	if *add != "" {
		if err := checkProductAccess(ctx, api, token, *add); err != nil {
			return nil, &CLIError{
				Code:    "PRODUCT_NOT_ADDED",
				Message: fmt.Sprintf("%s did not grant %s: %v", it.Name, *add, err),
				exit:    exitError,
			}
		}
		it.Products = append(it.Products, *add)
		st.Put(it)
		if err := a.saveState(st); err != nil {
			return nil, err
		}
		v.Products = it.Products
	}
	return &result{body: map[string]any{"env": a.Env, "reconnected": v}, message: linkedMessage("Reconnected", []itemView{v})}, nil
}

// readOnlyProducts are the products fin may request or add.
var readOnlyProducts = map[string]bool{"transactions": true, "investments": true}

// checkProductAccess calls the product's endpoint once, which is also what
// adds the product to the Item on Plaid's side. Data that is still being
// pulled counts as access.
func checkProductAccess(ctx context.Context, api Plaid, token, product string) error {
	var err error
	switch product {
	case "investments":
		_, err = api.InvestmentsHoldingsGet(ctx, token)
	case "transactions":
		_, err = api.TransactionsSync(ctx, token, "", 1)
	}
	if plaid.IsCode(err, "PRODUCT_NOT_READY") {
		return nil
	}
	return err
}

func toItemErrorAsCLI(it state.Item, err error) error {
	ie := toItemError(it, err)
	return newErr(ie.Code, "%s: %s", it.Name, ie.Message)
}

func (a *App) linkTokenRequest() plaid.LinkTokenCreateRequest {
	return plaid.LinkTokenCreateRequest{
		ClientName:   "fin",
		Language:     "en",
		CountryCodes: []string{"US"},
		User:         plaid.LinkUser{ClientUserID: clientUserID},
		HostedLink:   &plaid.HostedLink{},
	}
}

// runHostedLink creates a Hosted Link session, prints its URL to stderr, and
// polls /link/token/get until a session finishes or the timeout passes.
func (a *App) runHostedLink(ctx context.Context, api Plaid, req plaid.LinkTokenCreateRequest, open bool, timeout time.Duration) (*plaid.LinkSession, error) {
	tok, err := api.LinkTokenCreate(ctx, req)
	if err != nil {
		return nil, err
	}
	if tok.HostedLinkURL == "" {
		return nil, newErr("HOSTED_LINK_UNAVAILABLE", "Plaid returned no Hosted Link URL; Hosted Link may not be enabled for this client")
	}
	if a.showSpinner() {
		ui.Print(a.Stderr, ui.Line(ui.Plain, "Continue in Plaid Link:")+"\n  "+ui.Accent.Underline(true).Render(tok.HostedLinkURL)+"\n")
	} else {
		fmt.Fprintf(a.Stderr, "Open this URL to continue in Plaid Link:\n%s\n", tok.HostedLinkURL)
	}
	if open {
		if err := a.OpenURL(tok.HostedLinkURL); err != nil {
			fmt.Fprintf(a.Stderr, "could not open a browser: %v\n", err)
		}
	}

	deadline := a.Now().Add(timeout)
	if !tok.Expiration.IsZero() && tok.Expiration.Before(deadline) {
		deadline = tok.Expiration
	}
	if !a.showSpinner() {
		fmt.Fprintln(a.Stderr, "Waiting for Link to finish...")
		return a.pollLink(ctx, api, tok.LinkToken, deadline)
	}
	var sess *plaid.LinkSession
	err = ui.Wait(ctx, a.Stdin, a.Stderr, "Waiting for you to finish in the browser…", func(ctx context.Context) error {
		var err error
		sess, err = a.pollLink(ctx, api, tok.LinkToken, deadline)
		return err
	})
	return sess, err
}

func (a *App) pollLink(ctx context.Context, api Plaid, linkToken string, deadline time.Time) (*plaid.LinkSession, error) {
	for {
		got, err := api.LinkTokenGet(ctx, linkToken)
		if err != nil {
			return nil, err
		}
		if a.Debug {
			a.debugSessions(got.LinkSessions)
		}
		if sess := finishedSession(got.LinkSessions); sess != nil {
			return sess, nil
		}
		if !a.Now().Before(deadline) {
			return nil, newErr("LINK_TIMEOUT", "Link did not finish before %s", deadline.Local().Format(time.Kitchen))
		}
		select {
		case <-ctx.Done():
			return nil, asCLIError(ctx.Err())
		case <-time.After(a.PollInterval):
		}
	}
}

// debugSessions prints what Link reports for each session, without tokens.
func (a *App) debugSessions(sessions []plaid.LinkSession) {
	for _, s := range sessions {
		var events []string
		for _, e := range s.Events {
			events = append(events, e.EventName)
		}
		fmt.Fprintf(a.Stderr, "debug: session %s finished_at=%v on_success=%t exit=%t on_exit=%t item_add_results=%d events=%v\n",
			s.LinkSessionID, s.FinishedAt, s.OnSuccess != nil, s.Exit != nil, s.OnExit != nil, len(publicTokens(&s)), events)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(a.Stderr, "debug: no sessions yet")
	}
}

// finishedSession returns the session that ended, preferring one that added
// an Item, so an abandoned first attempt followed by a successful one still
// counts as success.
func finishedSession(sessions []plaid.LinkSession) *plaid.LinkSession {
	var finished *plaid.LinkSession
	for i := range sessions {
		s := &sessions[i]
		if len(publicTokens(s)) > 0 {
			return s
		}
		if sessionEnded(s) {
			finished = s
		}
	}
	return finished
}

// sessionEnded reports whether Link is done with a session. Plaid documents
// finished_at as set only "if available", so success and exit signals count too.
func sessionEnded(s *plaid.LinkSession) bool {
	if s.FinishedAt != nil || s.OnSuccess != nil || s.Exit != nil || s.OnExit != nil {
		return true
	}
	for _, e := range s.Events {
		if e.EventName == "HANDOFF" || e.EventName == "EXIT" {
			return true
		}
	}
	return false
}

// publicTokens are the new Items a session added. Update mode adds none.
func publicTokens(s *plaid.LinkSession) []string {
	var tokens []string
	if s.Results != nil {
		for _, r := range s.Results.ItemAddResults {
			if r.PublicToken != "" {
				tokens = append(tokens, r.PublicToken)
			}
		}
	}
	if len(tokens) == 0 && s.OnSuccess != nil && s.OnSuccess.PublicToken != "" {
		tokens = append(tokens, s.OnSuccess.PublicToken)
	}
	return tokens
}

// sessionExit is the exit details from either the current or legacy field.
func sessionExit(s *plaid.LinkSession) *plaid.LinkExit {
	if s.Exit != nil {
		return s.Exit
	}
	return s.OnExit
}

func linkExitErr(sess *plaid.LinkSession) error {
	exit := sessionExit(sess)
	if exit != nil && exit.Error != nil {
		e := exit.Error
		return &CLIError{Code: "LINK_FAILED", Message: e.Message, Details: map[string]any{"plaid_error_code": e.Code}, exit: exitError}
	}
	status := ""
	if exit != nil {
		status = exit.Metadata.Status
	}
	return &CLIError{Code: "LINK_EXITED", Message: "Link was closed before an account was linked", Details: map[string]any{"status": status}, exit: exitError}
}

// addItem exchanges a public token, stores the access token in the Keychain
// and records the Item in state.
func (a *App) addItem(ctx context.Context, api Plaid, st *state.State, kind, publicToken string, inst *plaid.Institution) (itemView, error) {
	ex, err := api.ItemPublicTokenExchange(ctx, publicToken)
	if err != nil {
		return itemView{}, err
	}
	if err := a.Secrets.Set(tokenAccount(a.Env, ex.ItemID), ex.AccessToken); err != nil {
		return itemView{}, newErr("KEYCHAIN_ERROR", "Item %s was created but its access token could not be stored: %v", ex.ItemID, err)
	}
	ig, err := api.ItemGet(ctx, ex.AccessToken)
	if err != nil {
		return itemView{}, err
	}
	if inst == nil || inst.Name == "" {
		inst = &plaid.Institution{}
		if id := deref(ig.Item.InstitutionID); id != "" {
			if got, err := api.InstitutionGetByID(ctx, id); err == nil {
				inst = got
			} else {
				inst.InstitutionID = id
			}
		}
	}
	if inst.Name == "" {
		inst.Name = inst.InstitutionID
	}
	it := state.Item{
		Name:            st.UniqueName(string(a.Env), inst.Name),
		ItemID:          ex.ItemID,
		Env:             string(a.Env),
		Kind:            kind,
		Products:        itemProducts(ig.Item.Products, linkProducts[kind]),
		InstitutionID:   inst.InstitutionID,
		InstitutionName: inst.Name,
		LinkedAt:        a.Now().UTC(),
	}
	st.Put(it)
	if err := a.saveState(st); err != nil {
		return itemView{}, err
	}
	v := newItemView(it)
	v.applyItemGet(it, ig)
	return v, nil
}

func firstOr(xs []string, def string) string {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}

// itemProducts is what the Item actually has, per /item/get, limited to the
// products fin reads. If Plaid reports none yet, it falls back to the
// required product.
func itemProducts(fromPlaid []string, lk linkKind) []string {
	var out []string
	for _, p := range fromPlaid {
		if readOnlyProducts[p] && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{lk.required}
	}
	slices.Sort(out)
	return out
}

// linkedMessage is the one-line confirmation people see after Link.
func linkedMessage(verb string, items []itemView) string {
	var lines []string
	for _, v := range items {
		line := fmt.Sprintf("%s %s as %s", verb, ui.Bold.Render(v.Institution), ui.Accent.Render(v.Name))
		if len(v.Products) > 0 {
			line += ui.Muted.Render(" · " + strings.Join(v.Products, ", "))
		}
		if v.ConsentExpiresAt != nil {
			line += ui.Muted.Render(" · consent until " + fmtDate(v.ConsentExpiresAt))
		}
		lines = append(lines, ui.Line(healthTone(v.Health), line))
	}
	return strings.Join(lines, "\n")
}

func (a *App) confirm(msg string) error {
	if !a.IsTerminal() {
		return newErr("CONFIRMATION_REQUIRED", "%s Rerun with --yes once the user has agreed.", msg)
	}
	fmt.Fprintf(a.Stderr, "%s Continue? [y/N] ", msg)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return newErr("ABORTED", "cancelled; no Item was linked")
}

// cmdSandboxLink links a sandbox Item without the Link UI. It exists for the
// end-to-end script and refuses to run outside sandbox.
func (a *App) cmdSandboxLink(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("sandbox-link", flag.ContinueOnError)
	institution := fs.String("institution", sandboxInstitution, "sandbox institution_id")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if a.Env != plaid.Sandbox {
		return nil, newErr("SANDBOX_ONLY", "sandbox-link only runs with PLAID_ENV=sandbox")
	}
	if _, ok := linkProducts[firstOr(pos, "")]; !ok || len(pos) != 1 {
		return nil, usageErr("usage: fin sandbox-link bank|brokerage [--institution ins_109508]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	api, err := a.plaid()
	if err != nil {
		return nil, err
	}
	pub, err := api.SandboxPublicTokenCreate(ctx, *institution, []string{linkProducts[pos[0]].required})
	if err != nil {
		return nil, err
	}
	v, err := a.addItem(ctx, api, st, pos[0], pub, nil)
	if err != nil {
		return nil, err
	}
	body := linkBody{Env: string(a.Env), Linked: []itemView{v}, SlotsUsed: len(st.ForEnv(string(a.Env))), SlotsTotal: SlotsTotal}
	return &result{body: body, message: linkedMessage("Linked", body.Linked)}, nil
}

// cmdSandboxResetLogin forces a sandbox Item into ITEM_LOGIN_REQUIRED.
func (a *App) cmdSandboxResetLogin(ctx context.Context, args []string) (*result, error) {
	if a.Env != plaid.Sandbox {
		return nil, newErr("SANDBOX_ONLY", "sandbox-reset-login only runs with PLAID_ENV=sandbox")
	}
	if len(args) != 1 {
		return nil, usageErr("usage: fin sandbox-reset-login <item>")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	it, err := a.findItem(st, args[0])
	if err != nil {
		return nil, err
	}
	token, err := a.Secrets.Get(tokenAccount(a.Env, it.ItemID))
	if err != nil {
		return nil, toItemErrorAsCLI(it, err)
	}
	api, err := a.plaid()
	if err != nil {
		return nil, err
	}
	if err := api.SandboxItemResetLogin(ctx, token); err != nil {
		return nil, err
	}
	return &result{body: map[string]any{"env": a.Env, "reset_login": it.Name}}, nil
}
