package fin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/sheets"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// googleTokenAccount holds the Google refresh token in the Keychain.
const googleTokenAccount = "google.refresh_token"

var sheetSteps = []string{
	"Sign in to Google",
	"Sync transactions",
	"Open the spreadsheet",
	"Write the Transactions and Accounts tabs",
}

const (
	stepSignIn = iota
	stepSync
	stepOpen
	stepWrite
)

type sheetBody struct {
	Env           plaid.Env      `json:"env"`
	SpreadsheetID string         `json:"spreadsheet_id"`
	URL           string         `json:"url"`
	Created       bool           `json:"created"`
	Transactions  int            `json:"transactions"`
	Accounts      int            `json:"accounts"`
	RetailerItems map[string]int `json:"retailer_items,omitempty"` // items written, by retailer
	Sync          []syncView     `json:"sync"`
	Errors        []ItemError    `json:"errors"`
}

func (a *App) cmdSheet(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("sheet", flag.ContinueOnError)
	open := fs.Bool("open", false, "open the spreadsheet when it is written")
	fresh := fs.Bool("new", false, "start a new spreadsheet instead of updating the current one")
	login := fs.Bool("login", false, "sign in to Google again")
	if pos, err := parseArgs(fs, args); err != nil {
		return nil, err
	} else if len(pos) > 0 {
		return nil, usageErr("usage: fin sheet [--open] [--new] [--login]")
	}
	if a.Google.ID == "" {
		return nil, newErr("GOOGLE_NOT_CONFIGURED", "this build of fin has no Google client; set FIN_GOOGLE_CLIENT_ID and FIN_GOOGLE_CLIENT_SECRET")
	}
	st, items, api, err := a.readSetup("transactions")
	if err != nil {
		return nil, err
	}

	var body *sheetBody
	work := func(ctx context.Context, r ui.Reporter) (ui.FlowResult, error) {
		var err error
		body, err = a.writeSheet(ctx, r, st, items, api, *fresh, *login)
		if err != nil {
			return ui.FlowResult{}, err
		}
		msg := fmt.Sprintf("Your sheet has %d transactions and %d accounts. Run fin sheet again to refresh it.", body.Transactions, body.Accounts)
		return ui.FlowResult{Message: msg, URL: body.URL, OpenAs: "the spreadsheet"}, nil
	}

	if a.showSpinner() {
		opts := ui.FlowOptions{
			Title:    "fin → Google Sheets",
			Subtitle: "fin keeps a spreadsheet in your Google Drive with your transactions and balances. It can only see the spreadsheet it creates, nothing else in your Drive.",
			Steps:    sheetSteps,
			Open:     a.OpenURL,
			// A spreadsheet URL is 88 characters, and must not wrap to stay clickable.
			Width: 90,
		}
		a.onScreen = true
		_, err := ui.Flow(ctx, a.Stdin, a.Stderr, opts, work)
		a.onScreen = false
		if err != nil {
			if errors.Is(err, ui.ErrCancelled) || errors.Is(err, context.Canceled) {
				return nil, newErr("CANCELLED", "cancelled")
			}
			return nil, err
		}
	} else {
		var r ui.Reporter = ui.PlainReporter{W: a.Stderr, Steps: sheetSteps}
		if !a.human {
			r = linkOnlyReporter{a}
		}
		if _, err := work(ctx, r); err != nil {
			return nil, err
		}
		if *open {
			_ = a.OpenURL(body.URL)
		}
	}
	msg := ui.Line(ui.Good, fmt.Sprintf("Wrote %d transactions and %d accounts to ", body.Transactions, body.Accounts)) + ui.Link(body.URL, body.URL)
	return &result{body: body, message: msg, errors: body.Errors}, nil
}

// linkOnlyReporter prints only the sign-in URL to stderr, for agents: stdout
// stays one JSON object, and the user still sees where to sign in.
type linkOnlyReporter struct{ a *App }

func (r linkOnlyReporter) Start(int, string) {}
func (r linkOnlyReporter) Done(int, string)  {}
func (r linkOnlyReporter) Feed(string)       {}
func (r linkOnlyReporter) Link(label, url string) {
	fmt.Fprintf(r.a.Stderr, "%s\n%s\n", label, url)
}

func (a *App) writeSheet(ctx context.Context, r ui.Reporter, st *state.State, items []state.Item, api Plaid, fresh, login bool) (*sheetBody, error) {
	body := &sheetBody{Env: a.Env, Sync: []syncView{}, Errors: []ItemError{}, RetailerItems: map[string]int{}}

	r.Start(stepSignIn, "")
	token, err := a.Secrets.Get(googleTokenAccount)
	if err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return nil, newErr("KEYCHAIN_ERROR", "%v", err)
	}
	if token == "" || login {
		if token, err = a.googleLogin(ctx, r); err != nil {
			return nil, err
		}
		r.Done(stepSignIn, "signed in")
	} else {
		r.Done(stepSignIn, "already signed in")
	}

	r.Start(stepSync, "")
	var s *store.Store
	if len(items) > 0 || len(st.Retailers) > 0 {
		if s, err = a.openStore(ctx); err != nil {
			return nil, err
		}
		defer s.Close()
	}
	if len(items) > 0 {
		if body.Sync, body.Errors, err = a.syncStore(ctx, s, st, items, api); err != nil {
			return nil, err
		}
	}
	if len(st.Retailers) > 0 {
		_, shopErrs, err := a.syncRetailers(ctx, s, st)
		if err != nil {
			return nil, err
		}
		body.Errors = append(body.Errors, shopErrs...)
	}
	changed := 0
	for _, v := range body.Sync {
		changed += v.Changed + v.Removed
	}
	r.Done(stepSync, fmt.Sprintf("%d connections, %d changes", len(items), changed))

	r.Start(stepOpen, "")
	svc := sheets.New(ctx, a.Google, token, a.SheetsBase)
	sp, created, err := a.openSpreadsheet(ctx, svc, st, fresh)
	if sheets.IsAuthExpired(err) {
		return nil, &CLIError{Code: "GOOGLE_SIGNIN_EXPIRED", Message: "Google no longer accepts fin's sign-in; run fin sheet --login",
			Details: map[string]any{"action": "run fin sheet --login"}, exit: exitError}
	}
	if err != nil {
		return nil, newErr("GOOGLE_SHEETS_ERROR", "%v", err)
	}
	body.SpreadsheetID, body.URL, body.Created = sp.ID, sp.URL, created
	if created {
		r.Done(stepOpen, "created a new one")
	} else {
		r.Done(stepOpen, "")
	}

	r.Start(stepWrite, "")
	var txs []store.Transaction
	var accounts []store.Account
	if s != nil {
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = it.ItemID
		}
		from := cmpOr(st.Epoch, "0001-01-01")
		if txs, err = s.Transactions(ctx, store.Filter{ItemIDs: ids, From: from, To: "9999-12-31"}); err != nil {
			return nil, storeErr(err)
		}
		if accounts, err = s.Accounts(ctx, ids); err != nil {
			return nil, storeErr(err)
		}
	}
	tabs := []sheets.Tab{transactionsTab(txs), accountsTab(accounts)}
	for _, r := range retailers {
		if len(st.RetailerAccounts(r.ID, string(a.Env))) == 0 {
			continue
		}
		items, err := s.RetailerItems(ctx, r.ID, store.ItemFilter{Since: st.Epoch})
		if err != nil {
			return nil, storeErr(err)
		}
		tabs = append(tabs, itemsTab(r, items))
		body.RetailerItems[r.ID] = len(items)
	}
	for _, tab := range tabs {
		if err := svc.Write(ctx, sp, tab); err != nil {
			return nil, newErr("GOOGLE_SHEETS_ERROR", "write %s: %v", tab.Title, err)
		}
	}
	body.Transactions, body.Accounts = len(txs), len(accounts)
	r.Done(stepWrite, fmt.Sprintf("%d transactions, %d accounts", len(txs), len(accounts)))
	return body, nil
}

func (a *App) googleLogin(ctx context.Context, r ui.Reporter) (string, error) {
	token, err := sheets.Login(ctx, a.Google, func(url string) {
		r.Link("Sign in with Google in your browser to let fin create its spreadsheet.", url)
		_ = a.OpenURL(url)
	})
	if errors.Is(err, sheets.ErrConsentDenied) {
		return "", newErr("GOOGLE_SIGNIN_CANCELLED", "Google sign-in was cancelled; run fin sheet to try again")
	}
	if err != nil {
		return "", err
	}
	if err := a.Secrets.Set(googleTokenAccount, token); err != nil {
		return "", newErr("KEYCHAIN_ERROR", "%v", err)
	}
	return token, nil
}

// openSpreadsheet opens the environment's spreadsheet, or creates one when
// there is none, it was deleted, or fresh is set.
func (a *App) openSpreadsheet(ctx context.Context, svc *sheets.Service, st *state.State, fresh bool) (*sheets.Spreadsheet, bool, error) {
	if id := st.Sheets[string(a.Env)]; id != "" && !fresh {
		sp, err := svc.Get(ctx, id)
		if err == nil || !sheets.IsGone(err) {
			return sp, false, err
		}
	}
	title := "fin"
	if a.Env != plaid.Production {
		title = "fin (" + string(a.Env) + ")"
	}
	sp, err := svc.Create(ctx, title, []string{"Transactions", "Accounts"})
	if err != nil {
		return nil, false, err
	}
	if st.Sheets == nil {
		st.Sheets = map[string]string{}
	}
	st.Sheets[string(a.Env)] = sp.ID
	if err := a.saveState(st); err != nil {
		return nil, false, err
	}
	return sp, true, nil
}

func transactionsTab(txs []store.Transaction) sheets.Tab {
	tab := sheets.Tab{Title: "Transactions", Columns: []sheets.Column{
		{Name: "Date", Kind: sheets.Date},
		{Name: "Authorized", Kind: sheets.Date},
		{Name: "Item"},
		{Name: "Account"},
		{Name: "Mask"},
		{Name: "Description"},
		{Name: "Merchant"},
		{Name: "Amount", Kind: sheets.Money},
		{Name: "Currency"},
		{Name: "Category"},
		{Name: "Detailed category"},
		{Name: "Pending", Kind: sheets.Bool},
		{Name: "Channel"},
		{Name: "Transaction ID"},
	}, Rows: [][]any{}}
	for _, t := range txs {
		tab.Rows = append(tab.Rows, []any{
			t.Date, t.AuthorizedDate, t.Item, t.AccountName, t.AccountMask, t.Name, t.MerchantName, t.Amount,
			t.IsoCurrencyCode, readable(deref(t.Category)), t.CategoryDetailed, t.Pending, t.PaymentChannel, t.TransactionID,
		})
	}
	return tab
}

func accountsTab(accounts []store.Account) sheets.Tab {
	tab := sheets.Tab{Title: "Accounts", Columns: []sheets.Column{
		{Name: "Item"},
		{Name: "Institution"},
		{Name: "Account"},
		{Name: "Official name"},
		{Name: "Mask"},
		{Name: "Type"},
		{Name: "Subtype"},
		{Name: "Current", Kind: sheets.Money},
		{Name: "Available", Kind: sheets.Money},
		{Name: "Limit", Kind: sheets.Money},
		{Name: "Currency"},
		{Name: "Updated", Kind: sheets.Date},
	}, Rows: [][]any{}}
	for _, ac := range accounts {
		tab.Rows = append(tab.Rows, []any{
			ac.Item, ac.Institution, ac.Name, ac.OfficialName, ac.Mask, ac.Type, ac.Subtype,
			ac.Current, ac.Available, ac.Limit, ac.IsoCurrencyCode, ac.UpdatedAt,
		})
	}
	return tab
}

func itemsTab(r retailer, items []store.RetailerItem) sheets.Tab {
	tab := sheets.Tab{Title: r.Name + " items", Columns: []sheets.Column{
		{Name: "Date", Kind: sheets.Date},
		{Name: "Order"},
		{Name: "Item"},
		{Name: "Qty", Kind: sheets.Number},
		{Name: "Category"},
		{Name: "Cost", Kind: sheets.Money},
		{Name: r.Name + " account"},
		{Name: "Bank transaction"},
	}, Rows: [][]any{}}
	for _, it := range items {
		tab.Rows = append(tab.Rows, []any{it.Date, it.OrderID, it.Title, it.Quantity, readable(deref(it.Category)), it.Cost, it.Account, it.Transaction})
	}
	return tab
}

func cmpOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// readable turns a Plaid category such as FOOD_AND_DRINK into "food and drink".
func readable(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", " "))
}
