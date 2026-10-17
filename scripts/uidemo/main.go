// Command uidemo fills a fin database with a year of illustrative data and
// serves it with fin ui, for the screenshots scripts/ui-shots.sh takes for
// the README and the landing page. None of it is anyone's real money.
//
//	go run ./scripts/uidemo -dir /tmp/fin-demo ui
//
// The arguments after the flags are a fin command that takes --ui. Without a
// terminal, fin prints the page's URL as one line of JSON and serves until it
// is interrupted. The data is the same on every run.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kilianc/fin/internal/fin"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
)

// today is the last day of data; the pages are drawn as of that evening.
var today = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

const days = 366 // a year of history, today included

func main() {
	dir := flag.String("dir", "", "directory for the demo state and database (required)")
	flag.Parse()
	if *dir == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: uidemo -dir DIR <fin command> [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	statePath, dataDir := filepath.Join(*dir, "state.json"), filepath.Join(*dir, "data")
	if _, err := os.Stat(store.Path(dataDir, string(plaid.Production))); os.IsNotExist(err) {
		if err := seed(ctx, statePath, dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "uidemo:", err)
			os.Exit(1)
		}
	}
	evening := time.Date(today.Year(), today.Month(), today.Day(), 18, 4, 0, 0, time.Local)
	app := &fin.App{
		Version:   "demo",
		Env:       plaid.Production,
		StatePath: statePath,
		DataDir:   dataDir,
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Now:       func() time.Time { return evening },
	}
	os.Exit(app.Run(ctx, append(flag.Args(), "--ui")))
}

func ptr[T any](v T) *T { return &v }

func day(i int) time.Time { return today.AddDate(0, 0, i-(days-1)) }

func date(i int) string { return day(i).Format("2006-01-02") }

// --- the household ---

type account struct {
	plaid.Account
	balance []float64 // one per day, oldest first
}

type item struct {
	id, name, institution string
	investments           bool
	accounts              []*account
}

func acct(id, name, mask, typ, subtype string, limit *float64) *account {
	return &account{Account: plaid.Account{
		AccountID: id, Name: name, Mask: ptr(mask), Type: typ, Subtype: ptr(subtype),
		Balances: plaid.Balances{Limit: limit, IsoCurrencyCode: ptr("USD")},
	}}
}

var (
	checking  = acct("chase-chk", "Total Checking", "1180", "depository", "checking", nil)
	sapphire  = acct("chase-card", "Sapphire Preferred", "4242", "credit", "credit card", ptr(15000.0))
	savings   = acct("ally-sav", "Online Savings", "3391", "depository", "savings", nil)
	gold      = acct("amex-gold", "Gold Card", "1007", "credit", "credit card", nil)
	autoLoan  = acct("toyota-loan", "Auto loan", "6610", "loan", "auto", nil)
	brokerage = acct("fid-brk", "Brokerage", "5520", "investment", "brokerage", nil)
	roth      = acct("fid-roth", "Roth IRA", "0917", "investment", "roth", nil)
	k401      = acct("vg-401k", "401(k)", "7781", "investment", "401k", nil)
	crypto    = acct("cb-crypto", "Crypto", "2214", "investment", "crypto exchange", nil)
)

var items = []*item{
	{id: "item-chase", name: "chase", institution: "Chase", accounts: []*account{checking, sapphire}},
	{id: "item-ally", name: "ally", institution: "Ally Bank", accounts: []*account{savings}},
	{id: "item-amex", name: "amex", institution: "American Express", accounts: []*account{gold}},
	{id: "item-toyota", name: "toyota", institution: "Toyota Financial Services", accounts: []*account{autoLoan}},
	{id: "item-fidelity", name: "fidelity", institution: "Fidelity", investments: true, accounts: []*account{brokerage, roth}},
	{id: "item-vanguard", name: "vanguard", institution: "Vanguard", investments: true, accounts: []*account{k401}},
	{id: "item-coinbase", name: "coinbase", institution: "Coinbase", investments: true, accounts: []*account{crypto}},
}

// position is a security held in one account: a fixed quantity, a price
// that walks to today's over the year, and a cost basis set by its gain.
type position struct {
	sec        plaid.Security
	account    *account
	quantity   float64
	price      float64 // today
	yearReturn float64 // what the price did over the year
	volatility float64
	gain       float64 // unrealized, as a fraction of cost
	prices     []float64
}

func security(id, ticker, name, typ string, cash bool) plaid.Security {
	return plaid.Security{SecurityID: id, TickerSymbol: ptr(ticker), Name: ptr(name), Type: ptr(typ),
		IsCashEquivalent: ptr(cash), IsoCurrencyCode: ptr("USD")}
}

var positions = []*position{
	{sec: security("s-vti", "VTI", "Vanguard Total Stock Market ETF", "etf", false), account: brokerage, quantity: 342.118, price: 287.68, yearReturn: 0.14, volatility: 0.010, gain: 0.312},
	{sec: security("s-aapl", "AAPL", "Apple Inc.", "equity", false), account: brokerage, quantity: 74, price: 238.37, yearReturn: 0.22, volatility: 0.017, gain: 0.647},
	{sec: security("s-t28", "T 4.25 28", "US Treasury Note 4.25% 2028", "fixed income", false), account: brokerage, quantity: 20, price: 1004.10, yearReturn: 0.012, volatility: 0.0015, gain: 0.004},
	{sec: security("s-spaxx", "SPAXX", "Fidelity Government Money Market", "mutual fund", true), account: brokerage, quantity: 5844.31, price: 1, gain: 0},
	{sec: security("s-fxaix", "FXAIX", "Fidelity 500 Index Fund", "mutual fund", false), account: roth, quantity: 141.27, price: 214.07, yearReturn: 0.15, volatility: 0.010, gain: 0.284},
	{sec: security("s-fzilx", "FZILX", "Fidelity ZERO International Index", "mutual fund", false), account: roth, quantity: 1229.43, price: 14.616, yearReturn: 0.08, volatility: 0.009, gain: 0.072},
	{sec: security("s-vfiax", "VFIAX", "Vanguard 500 Index Admiral", "mutual fund", false), account: k401, quantity: 37.84, price: 598.29, yearReturn: 0.15, volatility: 0.010, gain: 0.229},
	{sec: security("s-vtiax", "VTIAX", "Vanguard Total International Stock Index Admiral", "mutual fund", false), account: k401, quantity: 860.12, price: 36.32, yearReturn: 0.08, volatility: 0.009, gain: 0.098},
	{sec: security("s-vbtlx", "VBTLX", "Vanguard Total Bond Market Index Admiral", "mutual fund", false), account: k401, quantity: 4251.06, price: 9.616, yearReturn: 0.01, volatility: 0.003, gain: -0.031},
	{sec: security("s-vmfxx", "VMFXX", "Vanguard Federal Money Market", "mutual fund", true), account: k401, quantity: 17620.29, price: 1, gain: 0},
	{sec: security("s-btc", "BTC", "Bitcoin", "cryptocurrency", false), account: crypto, quantity: 0.0563, price: 108390.76, yearReturn: 0.45, volatility: 0.032, gain: 0.415},
	{sec: security("s-eth", "ETH", "Ethereum", "cryptocurrency", false), account: crypto, quantity: 0.479, price: 4305.05, yearReturn: -0.12, volatility: 0.04, gain: -0.124},
}

// --- the year ---

var rng = rand.New(rand.NewPCG(2026, 10))

func between(lo, hi float64) float64 { return math.Round((lo+rng.Float64()*(hi-lo))*100) / 100 }

// bridge is a random walk pinned to zero on the first and last day.
func bridge(sigma float64) []float64 {
	w := make([]float64, days)
	for i := 1; i < days; i++ {
		w[i] = w[i-1] + rng.NormFloat64()*sigma
	}
	out := make([]float64, days)
	for i := range w {
		out[i] = w[i] - w[days-1]*float64(i)/float64(days-1)
	}
	return out
}

func balances() {
	for _, p := range positions {
		noise := bridge(p.volatility)
		p.prices = make([]float64, days)
		for i := range p.prices {
			t := float64(i-(days-1)) / float64(days-1) // -1 a year ago, 0 today
			p.prices[i] = math.Round(p.price*math.Exp(t*math.Log1p(p.yearReturn)+noise[i])*10000) / 10000
		}
	}
	for _, a := range []*account{brokerage, roth, k401, crypto} {
		a.balance = make([]float64, days)
		for _, p := range positions {
			if p.account == a {
				for i, price := range p.prices {
					a.balance[i] += p.quantity * price
				}
			}
		}
	}
	// Cash, cards and the loan: a shape over the year that ends on today's
	// balance to the cent.
	shape := func(a *account, end float64, f func(i int, d time.Time) float64) {
		a.balance = make([]float64, days)
		off := end - f(days-1, today)
		for i := range a.balance {
			a.balance[i] = math.Round((f(i, day(i))+off)*100) / 100
		}
	}
	shape(checking, 12480.22, func(i int, d time.Time) float64 {
		// Paid on the 1st and the 15th, spent down in between.
		since := d.Day() - 1
		if d.Day() >= 15 {
			since = d.Day() - 15
		}
		return 9000 + 2600*math.Exp(-float64(since)/6) + 6*float64(i)
	})
	shape(savings, 38200, func(i int, d time.Time) float64 {
		months := (d.Year()-day(0).Year())*12 + int(d.Month()) - int(day(0).Month())
		return 26000 + 1000*float64(months) + float64(i)*1.2
	})
	shape(sapphire, 1834.12, func(i int, d time.Time) float64 {
		// Paid off on the 26th, then built back up.
		since := (d.Day() + 31 - 26) % 31
		return 350 + 85*float64(since) + 160*math.Sin(float64(i)/3.1)
	})
	shape(gold, 2566.40, func(i int, d time.Time) float64 {
		since := (d.Day() + 31 - 9) % 31
		return 600 + 95*float64(since) + 120*math.Sin(float64(i)/2.3)
	})
	shape(autoLoan, 9999.48, func(i int, d time.Time) float64 {
		months := (d.Year()-day(0).Year())*12 + int(d.Month()) - int(day(0).Month())
		if d.Day() < 12 {
			months--
		}
		return 15400 - 450*float64(months)
	})
}

type spend struct {
	account      *account
	name, cat    string
	detailed     string
	lo, hi       float64
	merchantName string
}

func transactions() map[string][]plaid.Transaction {
	byDate := map[string][]plaid.Transaction{}
	n := 0
	add := func(i int, a *account, name, merchant, cat, detailed string, amount float64) {
		n++
		t := plaid.Transaction{
			TransactionID: fmt.Sprintf("demo-%05d", n), AccountID: a.AccountID, Amount: amount,
			IsoCurrencyCode: ptr("USD"), Date: date(i), Name: name, PaymentChannel: "in store",
			PersonalFinanceCategory: &plaid.PersonalFinanceCategory{Primary: cat, Detailed: detailed},
		}
		if merchant != "" {
			t.MerchantName = ptr(merchant)
		}
		byDate[t.Date] = append(byDate[t.Date], t)
	}
	often := []struct {
		perWeek float64
		pick    []spend
	}{
		{2.2, []spend{
			{gold, "WHOLE FOODS MARKET", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", 48, 142, "Whole Foods Market"},
			{gold, "TRADER JOE'S", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", 32, 96, "Trader Joe's"},
			{sapphire, "SAFEWAY", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", 22, 74, "Safeway"},
		}},
		{3.4, []spend{
			{sapphire, "BLUE BOTTLE COFFEE", "FOOD_AND_DRINK", "FOOD_AND_DRINK_COFFEE", 5.25, 9.5, "Blue Bottle Coffee"},
			{gold, "TARTINE BAKERY", "FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT", 14, 32, "Tartine"},
			{gold, "NOPA", "FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT", 72, 128, "Nopa"},
			{gold, "SOUVLA", "FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT", 24, 46, "Souvla"},
			{sapphire, "ZUNI CAFE", "FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT", 88, 164, "Zuni Café"},
		}},
		{1.6, []spend{
			{sapphire, "UBER *TRIP", "TRANSPORTATION", "TRANSPORTATION_TAXIS_AND_RIDE_SHARES", 12, 38, "Uber"},
			{sapphire, "LYFT *RIDE", "TRANSPORTATION", "TRANSPORTATION_TAXIS_AND_RIDE_SHARES", 11, 34, "Lyft"},
			{sapphire, "SHELL OIL", "TRANSPORTATION", "TRANSPORTATION_GAS", 44, 64, "Shell"},
		}},
		{1.0, []spend{
			{sapphire, "AMAZON.COM", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES", 18, 124, "Amazon"},
			{sapphire, "AMAZON.COM", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES", 12, 68, "Amazon"},
			{gold, "TARGET", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_SUPERSTORES", 24, 96, "Target"},
		}},
	}
	for i := 0; i < days; i++ {
		d := day(i)
		for _, o := range often {
			if rng.Float64() < o.perWeek/7 {
				s := o.pick[rng.IntN(len(o.pick))]
				add(i, s.account, s.name, s.merchantName, s.cat, s.detailed, between(s.lo, s.hi))
			}
		}
		switch d.Day() {
		case 1:
			add(i, checking, "MISSION BAY APTS RENT", "", "RENT_AND_UTILITIES", "RENT_AND_UTILITIES_RENT", 1950)
			add(i, checking, "ACME CORP PAYROLL", "", "INCOME", "INCOME_WAGES", -4925)
		case 3:
			add(i, checking, "TRANSFER TO ALLY SAVINGS", "", "TRANSFER_OUT", "TRANSFER_OUT_SAVINGS", 1000)
			add(i, savings, "TRANSFER FROM CHASE", "", "TRANSFER_IN", "TRANSFER_IN_SAVINGS", -1000)
		case 5:
			add(i, sapphire, "SPOTIFY", "Spotify", "ENTERTAINMENT", "ENTERTAINMENT_MUSIC_AND_AUDIO", 11.99)
			add(i, gold, "EQUINOX", "Equinox", "PERSONAL_CARE", "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS", 265)
		case 8:
			add(i, sapphire, "NYTIMES", "The New York Times", "ENTERTAINMENT", "ENTERTAINMENT_OTHER_ENTERTAINMENT", 25)
		case 10:
			add(i, checking, "PG&E", "PG&E", "RENT_AND_UTILITIES", "RENT_AND_UTILITIES_GAS_AND_ELECTRICITY", between(96, 168))
		case 12:
			add(i, gold, "NETFLIX.COM", "Netflix", "ENTERTAINMENT", "ENTERTAINMENT_TV_AND_MOVIES", 15.49)
			add(i, checking, "TOYOTA FINANCIAL PMT", "", "LOAN_PAYMENTS", "LOAN_PAYMENTS_CAR_PAYMENT", 450)
		case 13:
			add(i, checking, "COSTCO WHSE", "Costco", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_SUPERSTORES", between(132, 268))
		case 15:
			add(i, checking, "ACME CORP PAYROLL", "", "INCOME", "INCOME_WAGES", -4925)
		case 18:
			add(i, checking, "COMCAST XFINITY", "Xfinity", "RENT_AND_UTILITIES", "RENT_AND_UTILITIES_INTERNET_AND_CABLE", 89.99)
		case 20:
			add(i, sapphire, "APPLE.COM/BILL", "Apple", "GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", 2.99)
		case 26:
			add(i, checking, "CHASE CREDIT CRD AUTOPAY", "", "LOAN_PAYMENTS", "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT", 1800)
			add(i, sapphire, "AUTOMATIC PAYMENT - THANK YOU", "", "LOAN_PAYMENTS", "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT", -1800)
		}
		// A few trips over the year, and the odd big purchase.
		switch date(i) {
		case "2025-11-21", "2026-02-12", "2026-06-19", "2026-09-04":
			add(i, sapphire, "DELTA AIR LINES", "Delta", "TRAVEL", "TRAVEL_FLIGHTS", between(380, 720))
			add(i, sapphire, "AIRBNB", "Airbnb", "TRAVEL", "TRAVEL_LODGING", between(540, 1260))
		case "2025-12-19", "2026-07-02":
			add(i, sapphire, "ALASKA AIRLINES", "Alaska Airlines", "TRAVEL", "TRAVEL_FLIGHTS", between(420, 860))
			add(i, gold, "HOTEL ZETTA", "Hotel Zetta", "TRAVEL", "TRAVEL_LODGING", between(640, 1380))
		case "2025-12-06":
			add(i, sapphire, "APPLE STORE", "Apple", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ELECTRONICS", 1199)
		case "2026-08-14":
			add(i, sapphire, "FULLY", "Fully", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_FURNITURE_AND_HARDWARE", 1180)
		case "2026-10-04":
			add(i, sapphire, "THE HOME DEPOT", "The Home Depot", "HOME_IMPROVEMENT", "HOME_IMPROVEMENT_HARDWARE", 196.42)
		}
	}
	// The newest charge is still pending.
	add(days-1, sapphire, "BLUE BOTTLE COFFEE", "Blue Bottle Coffee", "FOOD_AND_DRINK", "FOOD_AND_DRINK_COFFEE", 6.75)
	last := byDate[date(days-1)]
	last[len(last)-1].Pending = true
	return byDate
}

func seed(ctx context.Context, statePath, dataDir string) error {
	balances()
	txs := transactions()
	s, err := store.Open(ctx, store.Path(dataDir, string(plaid.Production)))
	if err != nil {
		return err
	}
	defer s.Close()
	st := &state.State{Version: 1, Env: string(plaid.Production)}
	lastSync := time.Date(today.Year(), today.Month(), today.Day(), 17, 58, 0, 0, time.Local)
	for _, it := range items {
		product := "transactions"
		if it.investments {
			product = "investments"
		}
		st.Items = append(st.Items, state.Item{
			Name: it.name, ItemID: it.id, Env: string(plaid.Production), Kind: "bank", Products: []string{product},
			InstitutionName: it.institution, LinkedAt: day(0).AddDate(0, -2, 0), LastSync: ptr(lastSync),
		})
		for i := 0; i < days; i++ {
			synced := time.Date(day(i).Year(), day(i).Month(), day(i).Day(), 17, 58, 0, 0, time.Local)
			var accounts []plaid.Account
			for _, a := range it.accounts {
				acc := a.Account
				acc.Balances.Current = ptr(math.Round(a.balance[i]*100) / 100)
				if acc.Balances.Limit != nil {
					acc.Balances.Available = ptr(*acc.Balances.Limit - *acc.Balances.Current)
				}
				accounts = append(accounts, acc)
			}
			if !it.investments {
				var upserts []plaid.Transaction
				for _, t := range txs[date(i)] {
					for _, a := range it.accounts {
						if t.AccountID == a.AccountID {
							upserts = append(upserts, t)
						}
					}
				}
				if err := s.Apply(ctx, store.ItemSync{
					ItemID: it.id, Item: it.name, Institution: it.institution, Cursor: date(i),
					Status: "HISTORICAL_UPDATE_COMPLETE", SyncedAt: synced.UTC(), Day: date(i),
					Accounts: accounts, Upserts: upserts,
				}); err != nil {
					return err
				}
				continue
			}
			var secs []plaid.Security
			var holdings []plaid.Holding
			for _, p := range positions {
				for _, a := range it.accounts {
					if p.account != a {
						continue
					}
					sec := p.sec
					sec.ClosePrice = ptr(p.prices[i])
					secs = append(secs, sec)
					value := math.Round(p.quantity*p.prices[i]*100) / 100
					holdings = append(holdings, plaid.Holding{
						AccountID: a.AccountID, SecurityID: p.sec.SecurityID, Quantity: p.quantity,
						InstitutionPrice: p.prices[i], InstitutionPriceAsOf: ptr(date(i)), InstitutionValue: value,
						CostBasis: ptr(math.Round(p.quantity*p.price/(1+p.gain)*100) / 100), IsoCurrencyCode: ptr("USD"),
					})
				}
			}
			if err := s.ApplyInvestments(ctx, store.InvestmentSync{
				ItemID: it.id, Item: it.name, Institution: it.institution, Day: date(i), SyncedAt: synced.UTC(),
				Accounts: accounts, Securities: secs, Holdings: holdings, From: date(i),
			}); err != nil {
				return err
			}
		}
	}
	return st.Save(statePath)
}
