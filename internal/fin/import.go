package fin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/robinhood"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

type importView struct {
	Account string   `json:"account"`
	Item    string   `json:"item"`
	Files   []string `json:"files"`
	Rows    int      `json:"rows"`
	First   string   `json:"first"`
	Last    string   `json:"last"`
	store.RobinhoodImport
}

// cmdImport brings in history Plaid does not reach, from a report the
// institution lets you download.
func (a *App) cmdImport(ctx context.Context, args []string) (*result, error) {
	if len(args) == 0 || args[0] != "robinhood" {
		return nil, usageErr("usage: fin import robinhood --account X FILE...")
	}
	fs := flag.NewFlagSet("import robinhood", flag.ContinueOnError)
	account := fs.String("account", "", "the Robinhood account the reports are for: its name, last four digits or ID")
	files, err := parseArgs(fs, args[1:])
	if err != nil {
		return nil, err
	}
	if *account == "" || len(files) == 0 {
		return nil, usageErr("usage: fin import robinhood --account X FILE...")
	}
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	accounts, err := s.InvestmentAccounts(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	acct, err := pickInvestmentAccount(accounts, "robinhood", *account)
	if err != nil {
		return nil, err
	}
	view := importView{Account: acct.Name, Item: acct.Item, Files: files}
	var rows []robinhood.Activity
	var reports []store.RobinhoodReport
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, newErr("IMPORT_ERROR", "%v", err)
		}
		got, err := robinhood.ParseActivity(acct.AccountID, bytes.NewReader(b))
		if err != nil {
			return nil, newErr("IMPORT_ERROR", "%s: %v", path, err)
		}
		sum := sha256.Sum256(b)
		rep := store.RobinhoodReport{ID: hex.EncodeToString(sum[:16]), File: filepath.Base(path), Rows: len(got)}
		for _, r := range got {
			if rep.First == "" || r.Date < rep.First {
				rep.First = r.Date
			}
			rep.Last = max(rep.Last, r.Date)
		}
		reports = append(reports, rep)
		rows = append(rows, got...)
	}
	out, securities := []store.RobinhoodRow{}, []plaid.Security{}
	known := map[string]string{}
	keys := map[string]bool{}
	for _, r := range rows {
		// The same row in two overlapping reports has the same key.
		if keys[r.Key] {
			continue
		}
		keys[r.Key] = true
		view.Rows++
		if view.First == "" || r.Date < view.First {
			view.First = r.Date
		}
		view.Last = max(view.Last, r.Date)
		t := store.RobinhoodRow{
			Key: r.Key, ActivityDate: r.Date, ProcessDate: r.ProcessDate, SettleDate: r.SettleDate,
			Instrument: r.Ticker, Description: r.RawDescription, TransCode: r.Code,
			QuantityText: r.QuantityText, PriceText: r.PriceText, AmountText: r.AmountText,
			Name: r.Description + " · " + r.Code, Type: r.Type, Subtype: r.Subtype,
			Quantity: r.Quantity, Price: r.Price, Amount: r.Amount,
		}
		if r.Ticker != "" {
			id, ok := known[r.Ticker]
			if !ok {
				if id, err = s.SecurityByTicker(ctx, r.Ticker); err != nil {
					return nil, storeErr(err)
				}
				if id == "" {
					id = "fin-ticker-" + r.Ticker
					ticker, name := r.Ticker, r.Security
					securities = append(securities, plaid.Security{SecurityID: id, TickerSymbol: &ticker, Name: &name})
				}
				known[r.Ticker] = id
			}
			t.SecurityID = &id
		}
		out = append(out, t)
	}
	res, err := s.ImportRobinhood(ctx, acct, reports, out, securities, a.Now().UTC())
	if err != nil {
		return nil, storeErr(err)
	}
	view.RobinhoodImport = res
	msg := ui.Line(ui.Good, fmt.Sprintf("Imported %d rows into %s (%s): %d new, %d already imported", view.Rows, acct.Name, acct.Item, res.New, res.Known))
	if res.PlaidReplaced > 0 {
		msg += "\n" + ui.Muted.Render(fmt.Sprintf("%d of Plaid's rows for this account are now read from the reports instead; they stay in plaid_investment_transactions", res.PlaidReplaced))
	}
	return &result{body: view, message: msg}, nil
}

// pickInvestmentAccount finds one of an institution's investment accounts by
// ID, last four digits, name, or a word of its name.
func pickInvestmentAccount(accounts []store.InvestmentAccount, item, key string) (store.InvestmentAccount, error) {
	var names []string
	var matches []store.InvestmentAccount
	for _, a := range accounts {
		if !strings.EqualFold(a.Item, item) && !strings.HasPrefix(strings.ToLower(a.Item), item) {
			continue
		}
		names = append(names, a.Name)
		switch {
		case a.AccountID == key, a.Mask == key, strings.EqualFold(a.Name, key):
			return a, nil
		case strings.Contains(strings.ToLower(a.Name), strings.ToLower(key)):
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(names) == 0 {
		return store.InvestmentAccount{}, newErr("NO_ACCOUNT", "no %s investment accounts synced yet; link Robinhood and run fin sync first", item)
	}
	return store.InvestmentAccount{}, &CLIError{Code: "NO_SUCH_ACCOUNT", exit: exitUsage,
		Message: fmt.Sprintf("--account %q matches %d of the %s accounts; use one of: %s", key, len(matches), item, strings.Join(names, ", "))}
}
