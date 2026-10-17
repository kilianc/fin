package fin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// fin --ui serves the local database as pages on 127.0.0.1. The pages are
// plain HTML, CSS and JavaScript embedded in the binary; they draw their
// charts as inline SVG from the JSON endpoints below, which read the store
// read-only, one request at a time, so fin sync can still write between them.

//go:embed web
var webFiles embed.FS

var uiTemplate = template.Must(template.ParseFS(webFiles, "web/page.html"))

// uiPages maps each command that takes --ui to the page that shows it.
var uiPages = map[string]string{
	"ui":           "overview",
	"accounts":     "overview",
	"transactions": "spending",
	"holdings":     "portfolio",
	"sql":          "query",
}

var uiTitles = map[string]string{
	"overview":  "Overview",
	"spending":  "Spending",
	"portfolio": "Portfolio",
	"query":     "Query",
}

// uiCSP lets a page load only fin's own files, the Geist fonts included, and
// talk only to the server that served it: nothing on the page reaches the
// internet.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// uiQueryRows caps the rows a query sends to the page.
const uiQueryRows = 10000

// uiTarget is the page a command opens, with the command's flags as the
// page's query.
type uiTarget struct {
	page  string
	query url.Values
}

func (t uiTarget) path() string {
	p := "/"
	if t.page != "overview" {
		p += t.page
	}
	if q := t.query.Encode(); q != "" {
		p += "?" + q
	}
	return p
}

// uiRoute checks a command's flags as the command itself would, and maps
// them to the page that shows its data.
func (a *App) uiRoute(name string, args []string) (uiTarget, error) {
	t := uiTarget{page: uiPages[name], query: url.Values{}}
	switch name {
	case "", "ui":
		t.page = "overview"
		if len(args) > 0 {
			return t, usageErr("usage: fin ui")
		}
	case "accounts":
		fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
		live := fs.Bool("live", false, "")
		if pos, err := parseArgs(fs, args); err != nil {
			return t, err
		} else if len(pos) > 0 {
			return t, usageErr("usage: fin accounts --ui")
		}
		if *live {
			return t, usageErr("--live cannot be combined with --ui: the page shows the balances from the last sync")
		}
	case "transactions":
		fs := flag.NewFlagSet("transactions", flag.ContinueOnError)
		since := fs.String("since", "", "")
		until := fs.String("until", "", "")
		account := fs.String("account", "", "")
		if pos, err := parseArgs(fs, args); err != nil {
			return t, err
		} else if len(pos) > 0 {
			return t, usageErr("unexpected argument %q", pos[0])
		}
		for _, f := range []struct{ name, value string }{{"since", *since}, {"until", *until}} {
			if f.value == "" {
				continue
			}
			if _, err := parseDate(f.name, f.value); err != nil {
				return t, err
			}
			t.query.Set(f.name, f.value)
		}
		if *since != "" && *until != "" && *until < *since {
			return t, usageErr("--until %s is before --since %s", *until, *since)
		}
		if *account != "" {
			t.query.Set("account", *account)
		}
	case "holdings":
		fs := flag.NewFlagSet("holdings", flag.ContinueOnError)
		account := fs.String("account", "", "")
		if pos, err := parseArgs(fs, args); err != nil {
			return t, err
		} else if len(pos) > 0 {
			return t, usageErr("usage: fin holdings [--account X] --ui")
		}
		if *account != "" {
			t.query.Set("account", *account)
		}
	case "sql":
		fs := flag.NewFlagSet("sql", flag.ContinueOnError)
		pos, err := parseArgs(fs, args)
		if err != nil {
			return t, err
		}
		query := strings.TrimSpace(strings.Join(pos, " "))
		if query == "" {
			return t, usageErr(`usage: fin sql "select ..." --ui`)
		}
		t.query.Set("q", query)
	default:
		return t, usageErr("--ui works with fin ui, accounts, transactions, holdings and sql, not %s", name)
	}
	return t, nil
}

// serveUI serves the pages until ctx ends (Ctrl-C), after opening target in
// the browser.
func (a *App) serveUI(ctx context.Context, target uiTarget) int {
	if _, err := os.Stat(a.storePath()); errors.Is(err, os.ErrNotExist) {
		return a.fail(storeErr(store.ErrNotFound))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return a.fail(newErr("UI_ERROR", "start the local server: %v", err))
	}
	token, err := uiToken()
	if err != nil {
		ln.Close()
		return a.fail(newErr("UI_ERROR", "%v", err))
	}
	host := ln.Addr().String()
	srv := &http.Server{Handler: a.uiHandler(token, host), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	link := "http://" + host + "/" + token + target.path()
	opened := a.OpenURL != nil && a.OpenURL(link) == nil
	if a.human {
		how := "Open it in your browser."
		if opened {
			how = "Opened in your browser."
		}
		ui.Print(a.Stdout, fmt.Sprintf("  %s %s  %s\n    %s Bound to 127.0.0.1; nothing is uploaded.\n    %s\n",
			ui.Good.Style().Render("■"), ui.Bold.Render("fin ui"), ui.Link(link, link), how, ui.Muted.Render("Press Ctrl-C to stop.")))
	} else {
		// An agent or a script gets the URL as one line of JSON right away;
		// the server keeps running until it is stopped, so start it in the
		// background.
		_ = json.NewEncoder(a.Stdout).Encode(map[string]any{
			"url": link, "page": target.page, "opened": opened,
			"stop": "fin serves this page until it is interrupted (SIGINT or SIGTERM)",
		})
	}

	select {
	case <-ctx.Done():
	case err := <-served:
		return a.fail(newErr("UI_ERROR", "local server stopped: %v", err))
	}
	stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(stop)
	return exitOK
}

func uiToken() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// uiHandler answers only requests for host whose path starts with the
// token, so another site in the browser, or another user on this Mac,
// cannot read the pages.
func (a *App) uiHandler(token, host string) http.Handler {
	mux := http.NewServeMux()
	for page := range uiTitles {
		pattern := "GET /" + page
		if page == "overview" {
			pattern = "GET /{$}"
		}
		mux.HandleFunc(pattern, a.uiPage(page, host))
	}
	static, _ := fs.Sub(webFiles, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /api/overview", a.uiRead(a.uiOverview))
	mux.HandleFunc("GET /api/spending", a.uiRead(a.uiSpending))
	mux.HandleFunc("GET /api/portfolio", a.uiRead(a.uiPortfolio))
	mux.HandleFunc("POST /api/query", a.uiQuery(host))

	prefix := "/" + token
	inner := http.StripPrefix(prefix, mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", uiCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		if r.Host != host {
			http.Error(w, "fin ui answers only on "+host, http.StatusMisdirectedRequest)
			return
		}
		seg, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if subtle.ConstantTimeCompare([]byte(seg), []byte(token)) != 1 {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == prefix {
			u := *r.URL
			u.Path = prefix + "/"
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func (a *App) uiPage(page, host string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lastSync := "never"
		if st, err := a.loadState(); err == nil {
			var newest *time.Time
			for _, it := range a.itemsWith(st, "") {
				if it.LastSync != nil && (newest == nil || it.LastSync.After(*newest)) {
					newest = it.LastSync
				}
			}
			if newest != nil {
				lastSync = newest.Local().Format("2006-01-02 15:04")
			}
		}
		db := filepath.Base(a.storePath())
		data := map[string]any{
			"Page": page, "Title": uiTitles[page], "Host": host, "DB": db, "Env": string(a.Env),
			"LastSync": lastSync, "Version": a.Version,
			"Config": map[string]any{
				"page": page, "env": a.Env, "db": db, "host": host,
				"today": a.Now().Format(dateLayout), "version": a.Version,
			},
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := uiTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type uiReader func(ctx context.Context, q url.Values, s *store.Store, ids []string) (any, error)

// uiRead opens the store read-only for one request, with the IDs of this
// environment's Items, and answers with what read returns.
func (a *App) uiRead(read uiReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		body, err := func() (any, error) {
			st, err := a.loadState()
			if err != nil {
				return nil, err
			}
			var ids []string
			for _, it := range a.itemsWith(st, "") {
				ids = append(ids, it.ItemID)
			}
			s, err := store.OpenReadOnly(ctx, a.storePath())
			if err != nil {
				return nil, storeErr(err)
			}
			defer s.Close()
			body, err := read(ctx, r.URL.Query(), s, ids)
			if err != nil {
				var cerr *CLIError
				if !errors.As(err, &cerr) {
					err = storeErr(err)
				}
			}
			return body, err
		}()
		if err != nil {
			uiError(w, err)
			return
		}
		uiJSON(w, http.StatusOK, body)
	}
}

// uiJSON encodes body before it sends a status, so a value JSON cannot hold
// answers with an error rather than a 200 and an empty body.
func uiJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		b, _ = json.Marshal(map[string]any{"error": newErr("INTERNAL", "encode the response: %v", err)})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func uiError(w http.ResponseWriter, err error) {
	cerr := asCLIError(err)
	status := http.StatusInternalServerError
	switch cerr.Code {
	case "USAGE", "SQL_ERROR":
		status = http.StatusBadRequest
	case "NO_LOCAL_DATA":
		status = http.StatusNotFound
	case "STORE_BUSY":
		status = http.StatusServiceUnavailable
	}
	uiJSON(w, status, map[string]any{"error": cerr})
}

// --- overview ---

type uiAccount struct {
	Item        string    `json:"item"`
	Institution string    `json:"institution"`
	AccountID   string    `json:"account_id"`
	Name        string    `json:"name"`
	Mask        *string   `json:"mask"`
	Type        string    `json:"type"`
	Subtype     *string   `json:"subtype"`
	Balance     *float64  `json:"balance"` // what it adds to net worth: negative for what is owed
	Current     *float64  `json:"current"`
	Available   *float64  `json:"available"`
	Limit       *float64  `json:"limit"`
	Currency    *string   `json:"iso_currency_code"`
	UpdatedAt   string    `json:"updated_at"`
	History     []float64 `json:"history"` // Balance on each of the last 30 days kept, oldest first
}

type uiGroup struct {
	Key      string      `json:"key"`
	Total    float64     `json:"total"`
	Accounts []uiAccount `json:"accounts"`
}

type uiPoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

type uiMonth struct {
	Month  string  `json:"month"` // YYYY-MM
	Income float64 `json:"income"`
	Spent  float64 `json:"spent"`
}

// accountGroups orders accounts on the overview by Plaid account type.
var accountGroups = []string{"depository", "credit", "loan", "investment", "other"}

func owed(accountType string) bool { return accountType == "credit" || accountType == "loan" }

// notSpending are the categories that move money without spending it.
// Everything else counts, refunds included, which take spending back.
var notSpending = map[string]bool{"INCOME": true, "TRANSFER_IN": true, "TRANSFER_OUT": true, "LOAN_PAYMENTS": true}

func spends(t store.Transaction) bool { return !notSpending[deref(t.Category)] }

func (a *App) uiOverview(ctx context.Context, q url.Values, s *store.Store, ids []string) (any, error) {
	today := a.Now()
	accounts, err := s.Accounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	points, err := s.BalanceHistory(ctx, ids)
	if err != nil {
		return nil, err
	}
	days, byAccount := dailyValues(points, today.Format(dateLayout))

	groups := map[string]*uiGroup{}
	var assets, liabilities float64
	assetIDs, owedIDs := map[string]bool{}, map[string]bool{}
	for _, acc := range accounts {
		key := acc.Type
		if !slices.Contains(accountGroups, key) {
			key = "other"
		}
		v := uiAccount{
			Item: acc.Item, Institution: acc.Institution, AccountID: acc.AccountID, Name: acc.Name, Mask: acc.Mask,
			Type: acc.Type, Subtype: acc.Subtype, Current: acc.Current, Available: acc.Available, Limit: acc.Limit,
			Currency: acc.IsoCurrencyCode, UpdatedAt: acc.UpdatedAt, History: []float64{},
		}
		if acc.Current != nil {
			b := *acc.Current
			if owed(acc.Type) {
				b = -b
				liabilities -= b
				owedIDs[acc.AccountID] = true
			} else {
				assets += b
				assetIDs[acc.AccountID] = true
			}
			v.Balance = &b
		}
		hist := byAccount[acc.AccountID]
		for _, x := range hist[max(0, len(hist)-30):] {
			if !math.IsNaN(x) {
				v.History = append(v.History, cents(x))
			}
		}
		g := groups[key]
		if g == nil {
			g = &uiGroup{Key: key, Accounts: []uiAccount{}}
			groups[key] = g
		}
		if v.Balance != nil {
			g.Total += *v.Balance
		}
		g.Accounts = append(g.Accounts, v)
	}
	ordered := []*uiGroup{}
	for _, key := range accountGroups {
		if g := groups[key]; g != nil {
			g.Total = cents(g.Total)
			ordered = append(ordered, g)
		}
	}

	sum := func(keep map[string]bool, sign float64) []uiPoint {
		out := make([]uiPoint, len(days))
		for i, day := range days {
			var total float64
			for id, vals := range byAccount {
				if keep == nil || keep[id] {
					if x := vals[i]; !math.IsNaN(x) {
						total += x
					}
				}
			}
			out[i] = uiPoint{Date: day, Value: cents(sign * total)}
		}
		return out
	}

	// Income and spending for each of the last 12 months, this one included.
	start := time.Date(today.Year(), today.Month()-11, 1, 0, 0, 0, 0, time.UTC)
	txs, err := s.Transactions(ctx, store.Filter{ItemIDs: ids, From: start.Format(dateLayout), To: today.Format(dateLayout)})
	if err != nil {
		return nil, err
	}
	months := make([]uiMonth, 12)
	for i := range months {
		months[i].Month = start.AddDate(0, i, 0).Format("2006-01")
	}
	for _, t := range txs {
		i := slices.IndexFunc(months, func(m uiMonth) bool { return strings.HasPrefix(t.Date, m.Month) })
		switch {
		case i < 0:
		case deref(t.Category) == "INCOME":
			months[i].Income -= t.Amount
		case spends(t):
			months[i].Spent += t.Amount
		}
	}
	for i := range months {
		months[i].Income, months[i].Spent = cents(months[i].Income), cents(months[i].Spent)
	}

	historySince := ""
	if len(days) > 0 {
		historySince = days[0]
	}
	return map[string]any{
		"env": a.Env, "today": today.Format(dateLayout),
		"net_worth": cents(assets - liabilities), "assets": cents(assets), "liabilities": cents(liabilities),
		"asset_accounts": len(assetIDs), "liability_accounts": len(owedIDs),
		"groups": ordered, "accounts": len(accounts), "items": len(ids),
		"history":             sum(nil, 1),
		"assets_history":      sum(assetIDs, 1),
		"liabilities_history": sum(owedIDs, -1),
		"history_since":       historySince,
		"months":              months,
	}, nil
}

// dailyValues fills in every day from the first point to end, carrying each
// key's last value forward, and returns the days and each key's value on
// them: NaN before its first point.
func dailyValues(points []store.DayValue, end string) ([]string, map[string][]float64) {
	byKey := map[string][]float64{}
	if len(points) == 0 {
		return []string{}, byKey
	}
	if last := points[len(points)-1].Date; last > end {
		end = last
	}
	first, _ := time.Parse(dateLayout, points[0].Date)
	stop, _ := time.Parse(dateLayout, end)
	var days []string
	current := map[string]float64{}
	p := 0
	for d := first; !d.After(stop); d = d.AddDate(0, 0, 1) {
		day := d.Format(dateLayout)
		for ; p < len(points) && points[p].Date <= day; p++ {
			current[points[p].Key] = points[p].Value
			if _, ok := byKey[points[p].Key]; !ok {
				byKey[points[p].Key] = slices.Repeat([]float64{math.NaN()}, len(days))
			}
		}
		days = append(days, day)
		for key := range byKey {
			v, ok := current[key]
			if !ok {
				v = math.NaN()
			}
			byKey[key] = append(byKey[key], v)
		}
	}
	return days, byKey
}

func cents(v float64) float64 { return math.Round(v*100) / 100 }

// --- spending ---

type uiCategory struct {
	Category string  `json:"category"`
	Spent    float64 `json:"spent"`
	Compare  float64 `json:"compare"` // the same days of the period before
}

type uiTransaction struct {
	store.Transaction
	Spending bool             `json:"spending"`
	Items    []store.PaidItem `json:"items,omitempty"`
}

type uiPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days int    `json:"days"`
}

func newPeriod(from, to time.Time) uiPeriod {
	return uiPeriod{From: from.Format(dateLayout), To: to.Format(dateLayout), Days: daysBetween(from, to) + 1}
}

func daysBetween(from, to time.Time) int { return int(math.Round(to.Sub(from).Hours() / 24)) }

// comparePeriod is what a spending period is measured against: the whole
// month before, when the period is a month so far, else the same number of
// days just before it. axis is how many days the running total spans.
func comparePeriod(from, to time.Time) (cmpFrom, cmpTo time.Time, axis int, monthToDate bool) {
	if from.Day() == 1 && from.Year() == to.Year() && from.Month() == to.Month() {
		cmpFrom = from.AddDate(0, -1, 0)
		return cmpFrom, from.AddDate(0, 0, -1), from.AddDate(0, 1, -1).Day(), true
	}
	n := daysBetween(from, to) + 1
	return from.AddDate(0, 0, -n), from.AddDate(0, 0, -1), n, false
}

func (a *App) uiSpending(ctx context.Context, q url.Values, s *store.Store, ids []string) (any, error) {
	today := a.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	from, to := today.AddDate(0, 0, 1-today.Day()), today
	for _, f := range []struct {
		name string
		day  *time.Time
	}{{"since", &from}, {"until", &to}} {
		if v := q.Get(f.name); v != "" {
			d, err := time.Parse(dateLayout, v)
			if err != nil {
				return nil, usageErr("%s must be a date like 2026-01-31, got %q", f.name, v)
			}
			*f.day = d
		}
	}
	if to.Before(from) {
		return nil, usageErr("until %s is before since %s", to.Format(dateLayout), from.Format(dateLayout))
	}
	account := q.Get("account")
	cmpFrom, cmpTo, axis, monthToDate := comparePeriod(from, to)
	period, compare := newPeriod(from, to), newPeriod(cmpFrom, cmpTo)
	txs, err := s.Transactions(ctx, store.Filter{
		ItemIDs: ids, From: cmpFrom.Format(dateLayout), To: to.Format(dateLayout), Account: account,
	})
	if err != nil {
		return nil, err
	}

	daily, counts, cmpDaily := make([]float64, period.Days), make([]int, period.Days), make([]float64, compare.Days)
	cats := map[string]*uiCategory{}
	category := func(t store.Transaction) *uiCategory {
		c := deref(t.Category)
		if cats[c] == nil {
			cats[c] = &uiCategory{Category: c}
		}
		return cats[c]
	}
	shown := []uiTransaction{}
	var paid []string
	for _, t := range txs {
		d, err := time.Parse(dateLayout, t.Date)
		if err != nil {
			continue
		}
		if d.Before(from) {
			if i := daysBetween(cmpFrom, d); spends(t) && i >= 0 && i < compare.Days {
				cmpDaily[i] += t.Amount
				if i < period.Days {
					category(t).Compare += t.Amount
				}
			}
			continue
		}
		shown = append(shown, uiTransaction{Transaction: t, Spending: spends(t)})
		paid = append(paid, t.TransactionID)
		if i := daysBetween(from, d); spends(t) && i < period.Days {
			daily[i] += t.Amount
			counts[i]++
			category(t).Spent += t.Amount
		}
	}
	items, err := s.ItemsPaidBy(ctx, paid)
	if err != nil {
		return nil, err
	}
	byTx := map[string][]store.PaidItem{}
	for _, it := range items {
		byTx[it.TransactionID] = append(byTx[it.TransactionID], it)
	}
	for i := range shown {
		shown[i].Items = byTx[shown[i].TransactionID]
	}

	var spent, cmpToDate, cmpSpent float64
	for i := range daily {
		daily[i] = cents(daily[i])
		spent += daily[i]
	}
	for i := range cmpDaily {
		cmpDaily[i] = cents(cmpDaily[i])
		cmpSpent += cmpDaily[i]
		if i < period.Days {
			cmpToDate += cmpDaily[i]
		}
	}
	categories := []uiCategory{}
	for _, c := range cats {
		c.Spent, c.Compare = cents(c.Spent), cents(c.Compare)
		if c.Spent != 0 || c.Compare != 0 {
			categories = append(categories, *c)
		}
	}
	slices.SortFunc(categories, func(x, y uiCategory) int {
		if x.Spent != y.Spent {
			if x.Spent > y.Spent {
				return -1
			}
			return 1
		}
		return strings.Compare(x.Category, y.Category)
	})
	return map[string]any{
		"env": a.Env, "today": today.Format(dateLayout), "account": account,
		"period": period, "compare": compare, "axis_days": axis, "month_to_date": monthToDate,
		"spent": cents(spent), "compare_spent": cents(cmpSpent), "compare_to_date": cents(cmpToDate),
		"daily": daily, "daily_count": counts, "compare_daily": cmpDaily,
		"categories": categories, "transactions": shown,
	}, nil
}

// --- portfolio ---

type uiHolding struct {
	store.Holding
	Class string `json:"class"`
}

// assetClass sorts a holding into the classes the allocation shows, by the
// security type Plaid reports.
func assetClass(h store.Holding, cash map[string]bool) string {
	switch t := strings.ToLower(deref(h.SecurityType)); {
	case cash[h.SecurityID] || t == "cash":
		return "cash"
	case t == "equity":
		return "stocks"
	case t == "etf" || t == "mutual fund":
		return "funds"
	case t == "fixed income":
		return "bonds"
	case t == "cryptocurrency":
		return "crypto"
	}
	return "other"
}

func (a *App) uiPortfolio(ctx context.Context, q url.Values, s *store.Store, ids []string) (any, error) {
	account := q.Get("account")
	holdings, err := s.Holdings(ctx, ids, account)
	if err != nil {
		return nil, err
	}
	cash, err := s.CashEquivalents(ctx)
	if err != nil {
		return nil, err
	}
	points, err := s.HoldingsHistory(ctx, ids, account)
	if err != nil {
		return nil, err
	}
	out := make([]uiHolding, len(holdings))
	var total, basis, basisValue float64
	accounts := map[string]bool{}
	asOf := ""
	for i, h := range holdings {
		out[i] = uiHolding{Holding: h, Class: assetClass(h, cash)}
		total += h.Value
		if h.CostBasis != nil {
			basis += *h.CostBasis
			basisValue += h.Value
		}
		accounts[h.AccountID] = true
		asOf = max(asOf, h.Date)
	}
	slices.SortStableFunc(out, func(x, y uiHolding) int {
		if x.Value > y.Value {
			return -1
		} else if x.Value < y.Value {
			return 1
		}
		return 0
	})

	days, byItem := dailyValues(points, a.Now().Format(dateLayout))
	history := make([]uiPoint, len(days))
	for i, day := range days {
		var v float64
		for _, vals := range byItem {
			if !math.IsNaN(vals[i]) {
				v += vals[i]
			}
		}
		history[i] = uiPoint{Date: day, Value: cents(v)}
	}
	// The snapshot before the latest one, to say what changed since.
	var previous any
	var snapshots []string
	for _, p := range points {
		if !slices.Contains(snapshots, p.Date) {
			snapshots = append(snapshots, p.Date)
		}
	}
	if len(snapshots) >= 2 {
		prev := snapshots[len(snapshots)-2]
		if i := slices.Index(days, prev); i >= 0 {
			previous = history[i]
		}
	}
	return map[string]any{
		"env": a.Env, "account": account, "as_of": asOf,
		"total": cents(total), "cost_basis": cents(basis), "basis_value": cents(basisValue),
		"accounts": len(accounts), "holdings": out, "history": history, "previous": previous,
	}, nil
}

// --- query ---

// querySQL runs one query the way fin sql does: on a connection that cannot
// write the database or read or write any other file. It reads at most limit
// rows; zero means all of them.
func (a *App) querySQL(ctx context.Context, query string, limit int) (*store.QueryResult, []store.SyncInfo, error) {
	s, err := store.OpenReadOnly(ctx, a.storePath())
	if err != nil {
		return nil, nil, storeErr(err)
	}
	defer s.Close()
	res, err := s.QueryLimit(ctx, query, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, nil, err
		}
		return nil, nil, newErr("SQL_ERROR", "%v", err)
	}
	synced, err := s.Syncs(ctx)
	if err != nil {
		return nil, nil, storeErr(err)
	}
	return res, synced, nil
}

func (a *App) uiQuery(host string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+host {
			http.Error(w, "cross-origin request", http.StatusForbidden)
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		var req struct {
			SQL string `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			uiError(w, usageErr("the request is not JSON: %v", err))
			return
		}
		query := strings.TrimSpace(req.SQL)
		if query == "" {
			uiError(w, usageErr("the query is empty"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		start := time.Now()
		res, synced, err := a.querySQL(ctx, query, uiQueryRows)
		if err != nil {
			uiError(w, err)
			return
		}
		for _, row := range res.Rows {
			for i, v := range row {
				row[i] = jsonSafe(v)
			}
		}
		uiJSON(w, http.StatusOK, map[string]any{
			"columns": uniqueNames(res.Columns), "types": res.Types, "rows": res.Rows,
			"row_count": len(res.Rows), "truncated": res.Truncated,
			"elapsed_ms": time.Since(start).Milliseconds(), "synced": synced,
		})
	}
}

// jsonSafe writes the floats JSON has no number for (NaN and the
// infinities) as text, inside lists and structs too.
func jsonSafe(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprint(x)
		}
	case float32:
		return jsonSafe(float64(x))
	case []any:
		for i := range x {
			x[i] = jsonSafe(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = jsonSafe(x[k])
		}
	}
	return v
}
