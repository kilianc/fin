package fin

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// --- accounts ---

type accountView struct {
	Item         string         `json:"item"`
	Institution  string         `json:"institution"`
	AccountID    string         `json:"account_id"`
	Name         string         `json:"name"`
	OfficialName *string        `json:"official_name"`
	Mask         *string        `json:"mask"`
	Type         string         `json:"type"`
	Subtype      *string        `json:"subtype"`
	Balances     plaid.Balances `json:"balances"`
}

func (a *App) cmdAccounts(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	live := fs.Bool("live", false, "fetch live balances from the institution (slower, billed per call)")
	if pos, err := parseArgs(fs, args); err != nil {
		return nil, err
	} else if len(pos) > 0 {
		return nil, usageErr("usage: fin accounts [--live]")
	}
	_, items, api, err := a.readSetup("")
	if err != nil {
		return nil, err
	}
	parts := make([][]accountView, len(items))
	errs := a.forEachItem(ctx, items, func(ctx context.Context, i int, token string) error {
		get := api.AccountsGet
		if *live {
			get = api.AccountsBalanceGet
		}
		resp, err := get(ctx, token)
		if err != nil {
			return err
		}
		for _, acc := range resp.Accounts {
			parts[i] = append(parts[i], accountView{
				Item:         items[i].Name,
				Institution:  items[i].InstitutionName,
				AccountID:    acc.AccountID,
				Name:         acc.Name,
				OfficialName: acc.OfficialName,
				Mask:         acc.Mask,
				Type:         acc.Type,
				Subtype:      acc.Subtype,
				Balances:     acc.Balances,
			})
		}
		return nil
	})
	accounts := flatten(parts)
	currencies := make([]*string, len(accounts))
	for i, v := range accounts {
		currencies[i] = v.Balances.IsoCurrencyCode
	}
	t := &ui.Table{
		Title:   fmt.Sprintf("Accounts · %d across %d Items", len(accounts), len(items)),
		Headers: []string{"Item", "Account", "Mask", "Type", "Current", "Available", "Limit", "Ccy"},
		Right:   []int{4, 5, 6},
		Footer:  "Balances from Plaid's daily refresh. Use --live to ask each bank now (billed per call).",
		Tone: func(row, col int) ui.Tone {
			if col == 2 || col == 3 || col == 7 {
				return ui.Dim
			}
			return ui.Plain
		},
	}
	if *live {
		t.Footer = "Live balances, straight from each institution."
	}
	for _, v := range accounts {
		t.Rows = append(t.Rows, []string{
			v.Item, v.Name, deref(v.Mask), strings.TrimSpace(deref(v.Subtype) + " " + v.Type),
			fmtMoney(v.Balances.Current), fmtMoney(v.Balances.Available), fmtMoney(v.Balances.Limit),
			deref(v.Balances.IsoCurrencyCode),
		})
	}
	if onlyUSD(currencies) {
		dropColumn(t, 7)
	}
	body := map[string]any{"env": a.Env, "live": *live, "accounts": accounts, "errors": errs}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- transactions ---

// transactionView is a stored transaction with its account and Item names.
type transactionView = store.Transaction

func (a *App) cmdTransactions(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("transactions", flag.ContinueOnError)
	since := fs.String("since", "", "first date, YYYY-MM-DD (required)")
	until := fs.String("until", "", "last date, YYYY-MM-DD (default today)")
	account := fs.String("account", "", "account_id, mask or name")
	from, to, err := a.dateRange(fs, args, since, until)
	if err != nil {
		return nil, err
	}
	st, items, api, err := a.readSetup("transactions")
	if err != nil {
		return nil, err
	}
	txs, syncList, errs := []transactionView{}, []syncView{}, []ItemError{}
	if len(items) > 0 {
		s, err := a.openStore(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if syncList, errs, err = a.syncStore(ctx, s, st, items, api); err != nil {
			return nil, err
		}
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = it.ItemID
		}
		if txs, err = s.Transactions(ctx, store.Filter{ItemIDs: ids, From: from, To: to, Account: *account}); err != nil {
			return nil, storeErr(err)
		}
	}
	var out, in float64
	t := &ui.Table{
		Title:   fmt.Sprintf("Transactions · %s → %s · %d", from, to, len(txs)),
		Headers: []string{"Date", "Item", "Account", "Description", "Amount", "Ccy", "Category"},
		Right:   []int{4},
		Tone: func(row, col int) ui.Tone {
			v := txs[row]
			switch {
			case col == 4 && v.Amount < 0:
				return ui.Good
			case v.Pending:
				return ui.Dim
			case col == 5 || col == 6:
				return ui.Dim
			}
			return ui.Plain
		},
	}
	for _, v := range txs {
		name := v.Name
		if v.MerchantName != nil {
			name = *v.MerchantName
		}
		if v.Pending {
			name += " (pending)"
		}
		if v.Amount >= 0 {
			out += v.Amount
		} else {
			in -= v.Amount
		}
		category := strings.ToLower(strings.ReplaceAll(deref(v.Category), "_", " "))
		t.Rows = append(t.Rows, []string{v.Date, v.Item, v.AccountName, truncate(name, 40), fmtNum2(-v.Amount), deref(v.IsoCurrencyCode), category})
	}
	t.Footer = fmt.Sprintf("Out %s · in %s  (spending shows as negative)", fmtNum2(out), fmtNum2(in))
	currencies := make([]*string, len(txs))
	for i, v := range txs {
		currencies[i] = v.IsoCurrencyCode
	}
	if onlyUSD(currencies) {
		dropColumn(t, 5)
	}
	body := map[string]any{
		"env": a.Env, "since": from, "until": to, "account": *account,
		"transactions": txs, "sync": syncList, "errors": errs,
	}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- holdings ---

type holdingView = store.Holding

func (a *App) cmdHoldings(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("holdings", flag.ContinueOnError)
	account := fs.String("account", "", "account_id, mask or name")
	if pos, err := parseArgs(fs, args); err != nil {
		return nil, err
	} else if len(pos) > 0 {
		return nil, usageErr("usage: fin holdings [--account X]")
	}
	holdings := []holdingView{}
	syncs, errs, err := a.readInvestments(ctx, func(s *store.Store, ids []string) (err error) {
		holdings, err = s.Holdings(ctx, ids, *account)
		return err
	})
	if err != nil {
		return nil, err
	}
	var total, basis float64
	t := &ui.Table{
		Headers: []string{"Item", "Account", "Security", "Quantity", "Price", "Value", "Cost basis", "Gain", "Lots"},
		Right:   []int{3, 4, 5, 6, 7, 8},
		Tone: func(row, col int) ui.Tone {
			v := holdings[row]
			if col == 7 && v.CostBasis != nil {
				if v.Value >= *v.CostBasis {
					return ui.Good
				}
				return ui.Bad
			}
			if col == 8 {
				return ui.Dim
			}
			return ui.Plain
		},
	}
	for _, v := range holdings {
		security := deref(v.Ticker)
		if security == "" {
			security = truncate(deref(v.SecurityName), 28)
		}
		gain := ""
		if v.CostBasis != nil {
			gain = fmtNum2(v.Value - *v.CostBasis)
			basis += *v.CostBasis
		}
		total += v.Value
		var lots []json.RawMessage
		_ = json.Unmarshal(v.TaxLots, &lots)
		t.Rows = append(t.Rows, []string{
			v.Item, v.AccountName, security, fmtNum(v.Quantity), fmtNum2(v.Price), fmtNum2(v.Value),
			fmtMoney(v.CostBasis), gain, fmtNum(float64(len(lots))),
		})
	}
	t.Title = fmt.Sprintf("Holdings · %d positions · %s", len(holdings), fmtNum2(total))
	t.Footer = fmt.Sprintf("Total value %s, cost basis %s where known.", fmtNum2(total), fmtNum2(basis))
	body := map[string]any{"env": a.Env, "holdings": holdings, "sync": syncs, "errors": errs}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- investment transactions ---

type investmentTransactionView = store.InvestmentTransaction

// invPageSize is Plaid's maximum page size for /investments/transactions/get.
var invPageSize = 500

func (a *App) cmdInvestments(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("investments", flag.ContinueOnError)
	since := fs.String("since", "", "first date, YYYY-MM-DD (required)")
	until := fs.String("until", "", "last date, YYYY-MM-DD (default today)")
	account := fs.String("account", "", "account_id, mask or name")
	from, to, err := a.dateRange(fs, args, since, until)
	if err != nil {
		return nil, err
	}
	txs := []investmentTransactionView{}
	syncs, errs, err := a.readInvestments(ctx, func(s *store.Store, ids []string) (err error) {
		txs, err = s.InvestmentTransactions(ctx, store.Filter{ItemIDs: ids, From: from, To: to, Account: *account})
		return err
	})
	if err != nil {
		return nil, err
	}
	t := &ui.Table{
		Title:   fmt.Sprintf("Investment transactions · %s → %s · %d", from, to, len(txs)),
		Headers: []string{"Date", "Item", "Account", "Type", "Security", "Quantity", "Price", "Amount"},
		Right:   []int{5, 6, 7},
		Tone: func(row, col int) ui.Tone {
			if col == 3 {
				return ui.Dim
			}
			return ui.Plain
		},
	}
	for _, v := range txs {
		security := deref(v.Ticker)
		if security == "" {
			security = truncate(deref(v.SecurityName), 28)
		}
		kind := v.Type
		if v.Subtype != "" && v.Subtype != v.Type {
			kind += " · " + v.Subtype
		}
		t.Rows = append(t.Rows, []string{
			v.Date, v.Item, v.AccountName, kind, security,
			fmtNum(v.Quantity), fmtNum2(v.Price), fmtNum2(v.Amount),
		})
	}
	body := map[string]any{
		"env": a.Env, "since": from, "until": to, "account": *account,
		"investment_transactions": txs, "sync": syncs, "errors": errs,
	}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- shared helpers ---

// readSetup loads state, picks the Items with product, and builds a client.
// With no matching Items it skips the Keychain entirely.
func (a *App) readSetup(product string) (*state.State, []state.Item, Plaid, error) {
	st, err := a.loadState()
	if err != nil {
		return nil, nil, nil, err
	}
	items := a.itemsWith(st, product)
	if len(items) == 0 {
		return st, items, nil, nil
	}
	api, err := a.plaid()
	if err != nil {
		return nil, nil, nil, err
	}
	return st, items, api, nil
}

func (a *App) dateRange(fs *flag.FlagSet, args []string, since, until *string) (string, string, error) {
	pos, err := parseArgs(fs, args)
	if err != nil {
		return "", "", err
	}
	if len(pos) > 0 {
		return "", "", usageErr("unexpected argument %q", pos[0])
	}
	if *since == "" {
		return "", "", usageErr("--since is required, e.g. --since 2026-01-01")
	}
	from, err := parseDate("since", *since)
	if err != nil {
		return "", "", err
	}
	to := a.Now().Format(dateLayout)
	if *until != "" {
		if to, err = parseDate("until", *until); err != nil {
			return "", "", err
		}
	}
	if to < from {
		return "", "", usageErr("--until %s is before --since %s", to, from)
	}
	return from, to, nil
}

func flatten[T any](parts [][]T) []T {
	out := []T{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func fmtNum2(v float64) string { return fmtMoney(&v) }

// onlyUSD reports whether every amount is in US dollars, so the currency
// column adds nothing.
func onlyUSD(codes []*string) bool {
	for _, c := range codes {
		if c != nil && *c != "USD" {
			return false
		}
	}
	return true
}

// dropColumn removes a column from a table, keeping tones and alignment
// pointed at the right cells.
func dropColumn(t *ui.Table, col int) {
	t.Headers = slices.Delete(slices.Clone(t.Headers), col, col+1)
	for i, row := range t.Rows {
		t.Rows[i] = slices.Delete(slices.Clone(row), col, col+1)
	}
	var right []int
	for _, c := range t.Right {
		switch {
		case c < col:
			right = append(right, c)
		case c > col:
			right = append(right, c-1)
		}
	}
	t.Right = right
	if tone := t.Tone; tone != nil {
		t.Tone = func(row, c int) ui.Tone {
			if c >= col {
				c++
			}
			return tone(row, c)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
