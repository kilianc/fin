package fin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/store"
)

func TestUIRejectsOtherOutputsAndCommands(t *testing.T) {
	ta := newTestApp(t, chase)
	for _, args := range [][]string{
		{"accounts", "--ui", "--json"},
		{"transactions", "--table", "--ui"},
		{"ui", "--json"},
		{"ui", "--table"},
		{"items", "--ui"},
		{"accounts", "--live", "--ui"},
		{"transactions", "--since", "2026-13-01", "--ui"},
		{"sql", "--ui"},
		{"ui", "extra"},
	} {
		// --table makes errors human-readable too, so look for the code.
		if code := ta.Run(context.Background(), args); code != exitUsage || !strings.Contains(ta.stderr.String(), "USAGE") {
			t.Errorf("%v: exit %d, stderr %s", args, code, ta.stderr)
		}
		ta.stderr.Reset()
	}
}

func TestUIRoutesKeepTheCommandsFlags(t *testing.T) {
	ta := newTestApp(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"ui"}, "/"},
		{nil, "/"},
		{[]string{"accounts"}, "/"},
		{[]string{"transactions", "--since", "2026-10-01", "--account", "4242"}, "/spending?account=4242&since=2026-10-01"},
		{[]string{"transactions"}, "/spending"},
		{[]string{"holdings", "--account", "ira"}, "/portfolio?account=ira"},
		{[]string{"sql", "select 1"}, "/query?q=select+1"},
	} {
		name := ""
		var rest []string
		if len(c.args) > 0 {
			name, rest = c.args[0], c.args[1:]
		}
		target, err := ta.uiRoute(name, rest)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if got := target.path(); got != c.want {
			t.Errorf("%v: path = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestUINeedsASyncFirst(t *testing.T) {
	ta := newTestApp(t, chase)
	if code, _ := ta.run(t, "ui"); code != exitError || ta.stderrJSON(t)["code"] != "NO_LOCAL_DATA" {
		t.Fatalf("exit %d, stderr %s", code, ta.stderr)
	}
}

// uiSession runs fin with args and returns the URL it opened in the
// browser, and stop, which presses Ctrl-C and returns what fin printed.
func uiSession(t *testing.T, ta *testApp, args ...string) (*url.URL, func() string) {
	t.Helper()
	ta.stdout.Reset()
	ta.stderr.Reset()
	opened := make(chan string, 1)
	ta.OpenURL = func(u string) error { opened <- u; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ta.Run(ctx, args) }()
	var once sync.Once
	stop := func() string {
		once.Do(func() {
			cancel()
			select {
			case code := <-done:
				if code != exitOK {
					t.Errorf("fin %v exit = %d after Ctrl-C; stderr %s", args, code, ta.stderr)
				}
			case <-time.After(5 * time.Second):
				t.Error("the server did not stop after Ctrl-C")
			}
		})
		return ta.stdout.String()
	}
	t.Cleanup(func() { stop() })
	select {
	case raw := <-opened:
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u, stop
	case code := <-done:
		t.Fatalf("fin %v exited %d before opening a page; stderr %s", args, code, ta.stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("no page was opened")
	}
	return nil, stop
}

func uiGet(t *testing.T, u string, into any) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("GET %s: %v\n%s", u, err, body)
		}
	}
	return resp.StatusCode
}

func uiPost(t *testing.T, u, origin, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestUIServesTheLocalDatabase(t *testing.T) {
	ta := newTestApp(t, chase, fido)
	cat := func(t plaid.Transaction, c string) plaid.Transaction {
		t.PersonalFinanceCategory = &plaid.PersonalFinanceCategory{Primary: c}
		return t
	}
	ta.fake.syncPages["tok-item-chase"] = func(cursor string, _ int) (*plaid.TransactionsSyncResponse, error) {
		return &plaid.TransactionsSyncResponse{
			Added: []plaid.Transaction{
				cat(tx("lunch", "card", "2026-10-02", 50), "FOOD_AND_DRINK"),
				cat(tx("refund", "card", "2026-10-03", -10), "FOOD_AND_DRINK"),
				cat(tx("pay", "chk", "2026-10-03", 200), "LOAN_PAYMENTS"),
				cat(tx("sep-lunch", "card", "2026-09-04", 30), "FOOD_AND_DRINK"),
				cat(tx("sep-late", "card", "2026-09-20", 70), "GENERAL_MERCHANDISE"),
				cat(tx("salary", "chk", "2026-09-15", -1000), "INCOME"),
			},
			Accounts: []plaid.Account{
				{AccountID: "chk", Name: "Checking", Mask: ptr("0001"), Type: "depository", Balances: plaid.Balances{Current: ptr(1500.0)}},
				{AccountID: "card", Name: "Sapphire", Mask: ptr("4242"), Type: "credit", Balances: plaid.Balances{Current: ptr(300.0), Limit: ptr(5000.0)}},
			},
			NextCursor: "c1", TransactionsUpdateStatus: "HISTORICAL_UPDATE_COMPLETE",
		}, nil
	}
	ta.fake.holdings["tok-item-fido"] = &plaid.HoldingsResponse{
		Accounts: []plaid.Account{{AccountID: "ira", Name: "Roth IRA", Type: "investment", Balances: plaid.Balances{Current: ptr(4000.0)}}},
		Securities: []plaid.Security{
			{SecurityID: "s-vti", TickerSymbol: ptr("VTI"), Type: ptr("etf")},
			{SecurityID: "s-mm", TickerSymbol: ptr("SPAXX"), Type: ptr("mutual fund"), IsCashEquivalent: ptr(true)},
		},
		Holdings: []plaid.Holding{
			{AccountID: "ira", SecurityID: "s-vti", Quantity: 10, InstitutionPrice: 300, InstitutionValue: 3000, CostBasis: ptr(2500.0)},
			{AccountID: "ira", SecurityID: "s-mm", Quantity: 1000, InstitutionPrice: 1, InstitutionValue: 1000},
		},
	}
	if code, _ := ta.run(t, "sync"); code != exitOK {
		t.Fatalf("sync exit %d; stderr %s", code, ta.stderr)
	}

	u, stop := uiSession(t, ta, "transactions", "--since", "2026-10-01", "--ui")
	if u.Hostname() != "127.0.0.1" || u.Path == "" || !strings.HasSuffix(u.Path, "/spending") || u.Query().Get("since") != "2026-10-01" {
		t.Fatalf("opened %s", u)
	}
	base := u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "spending")

	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), `data-page="spending"`) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Errorf("page: %d %s", resp.StatusCode, page)
	}
	// Nothing on the page may reach the internet, the fonts included.
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "http") || strings.Contains(string(page), "https://") {
		t.Errorf("the page loads from the internet: CSP %q", csp)
	}
	for _, static := range []string{"static/ui.js", "static/ui.css", "static/mascot.png", "static/fonts/Geist-latin.woff2", "static/fonts/GeistMono-latin.woff2"} {
		if code := uiGet(t, base+static, nil); code != http.StatusOK {
			t.Errorf("%s: %d", static, code)
		}
	}

	var ov struct {
		NetWorth    float64 `json:"net_worth"`
		Liabilities float64 `json:"liabilities"`
		Groups      []struct {
			Key   string  `json:"key"`
			Total float64 `json:"total"`
		} `json:"groups"`
		History []uiPoint `json:"history"`
		Months  []uiMonth `json:"months"`
	}
	if code := uiGet(t, base+"api/overview", &ov); code != http.StatusOK {
		t.Fatalf("overview: %d", code)
	}
	if ov.NetWorth != 1500-300+4000 || ov.Liabilities != 300 {
		t.Errorf("net worth %v, liabilities %v", ov.NetWorth, ov.Liabilities)
	}
	var keys []string
	for _, g := range ov.Groups {
		keys = append(keys, g.Key)
	}
	if !slices.Equal(keys, []string{"depository", "credit", "investment"}) {
		t.Errorf("groups = %v", keys)
	}
	if len(ov.History) != 1 || ov.History[0] != (uiPoint{Date: "2026-10-09", Value: 5200}) {
		t.Errorf("history = %v, want today's balances", ov.History)
	}
	if sep := ov.Months[10]; sep.Month != "2026-09" || sep.Income != 1000 || sep.Spent != 100 {
		t.Errorf("September = %+v", sep)
	}

	var sp struct {
		Spent         float64      `json:"spent"`
		CompareToDate float64      `json:"compare_to_date"`
		CompareSpent  float64      `json:"compare_spent"`
		AxisDays      int          `json:"axis_days"`
		Compare       uiPeriod     `json:"compare"`
		Complete      bool         `json:"compare_complete"`
		HistorySince  string       `json:"history_since"`
		Categories    []uiCategory `json:"categories"`
		Transactions  []struct {
			ID       string `json:"transaction_id"`
			Spending bool   `json:"spending"`
		} `json:"transactions"`
	}
	if code := uiGet(t, base+"api/spending?since=2026-10-01", &sp); code != http.StatusOK {
		t.Fatalf("spending: %d", code)
	}
	if sp.Spent != 40 || sp.CompareToDate != 30 || sp.CompareSpent != 100 || sp.AxisDays != 31 || sp.Compare.From != "2026-09-01" || sp.Compare.Days != 30 {
		t.Errorf("spending = %+v", sp)
	}
	// September 20 is past the ninth, so it is not in the to-date comparison.
	if len(sp.Categories) != 1 || sp.Categories[0] != (uiCategory{Category: "FOOD_AND_DRINK", Spent: 40, Compare: 30}) {
		t.Errorf("categories = %+v", sp.Categories)
	}
	if len(sp.Transactions) != 3 || sp.Transactions[0].ID != "pay" || sp.Transactions[0].Spending {
		t.Errorf("transactions = %+v, want October's three, the card payment not counted", sp.Transactions)
	}
	// The history starts on September 4, three days into September: still a
	// fair comparison for October so far.
	if !sp.Complete || sp.HistorySince != "2026-09-04" {
		t.Errorf("October against September: complete %v, history since %q", sp.Complete, sp.HistorySince)
	}
	// From September 1 the period before is late July and August, which
	// the store has nothing for.
	if code := uiGet(t, base+"api/spending?since=2026-09-01", &sp); code != http.StatusOK || sp.Complete || sp.HistorySince != "2026-09-04" {
		t.Errorf("September on: %d, complete %v, history since %q; want no comparison", code, sp.Complete, sp.HistorySince)
	}
	if code := uiGet(t, base+"api/spending?since=yesterday", nil); code != http.StatusBadRequest {
		t.Errorf("bad since: %d", code)
	}

	var pf struct {
		Total    float64 `json:"total"`
		Basis    float64 `json:"cost_basis"`
		Holdings []struct {
			Ticker string `json:"ticker"`
			Class  string `json:"class"`
		} `json:"holdings"`
	}
	if code := uiGet(t, base+"api/portfolio", &pf); code != http.StatusOK {
		t.Fatalf("portfolio: %d", code)
	}
	if pf.Total != 4000 || pf.Basis != 2500 || len(pf.Holdings) != 2 || pf.Holdings[0].Class != "funds" || pf.Holdings[1].Class != "cash" {
		t.Errorf("portfolio = %+v", pf)
	}

	origin := u.Scheme + "://" + u.Host
	code, out := uiPost(t, base+"api/query", origin, `{"sql": "select count(*) as n from transactions"}`)
	if code != http.StatusOK || out["rows"].([]any)[0].([]any)[0] != 6.0 || out["columns"].([]any)[0] != "n" {
		t.Errorf("query: %d %v", code, out)
	}
	if code, out := uiPost(t, base+"api/query", origin, `{"sql": "delete from transactions"}`); code != http.StatusBadRequest || out["error"].(map[string]any)["code"] != "SQL_ERROR" {
		t.Errorf("a write must fail like fin sql: %d %v", code, out)
	}
	if code, _ := uiPost(t, base+"api/query", "https://evil.example", `{"sql": "select 1"}`); code != http.StatusForbidden {
		t.Errorf("cross-origin query: %d", code)
	}
	// A huge result stops at the cap instead of filling memory.
	code, out = uiPost(t, base+"api/query", origin, `{"sql": "select * from range(100000000)"}`)
	if code != http.StatusOK || len(out["rows"].([]any)) != uiQueryRows || out["truncated"] != true {
		t.Errorf("huge query: %d, %d rows, truncated %v", code, len(out["rows"].([]any)), out["truncated"])
	}
	// NaN has no JSON number, even inside a list.
	code, out = uiPost(t, base+"api/query", origin, `{"sql": "select ['nan'::double, 1.5] as x"}`)
	if code != http.StatusOK || fmt.Sprint(out["rows"]) != "[[[NaN 1.5]]]" {
		t.Errorf("NaN in a list: %d %v", code, out)
	}

	wrong := origin + "/not-the-token/api/overview"
	if code := uiGet(t, wrong, nil); code != http.StatusNotFound {
		t.Errorf("wrong token: %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"api/overview", nil)
	req.Host = "attacker.example:80"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("wrong Host: %v %v", resp, err)
	}

	// Without a terminal, the URL comes as one line of JSON for an agent.
	var printed struct {
		URL    string `json:"url"`
		Page   string `json:"page"`
		Opened bool   `json:"opened"`
	}
	if err := json.Unmarshal([]byte(stop()), &printed); err != nil || printed.URL != u.String() || printed.Page != "spending" || !printed.Opened {
		t.Errorf("stdout = %+v, %v; want the URL as JSON", printed, err)
	}

	// In a terminal, a person gets the URL and how to stop.
	ta.StdoutIsTerminal = func() bool { return true }
	u, stop = uiSession(t, ta, "ui")
	if out := stop(); !strings.Contains(out, u.String()) || !strings.Contains(out, "Opened in your browser") || !strings.Contains(out, "Ctrl-C") {
		t.Errorf("stdout = %q, want the URL and how to stop", out)
	}
}

func TestDailyValuesCarriesEachKeyForward(t *testing.T) {
	days, byKey := dailyValues([]store.DayValue{
		{Date: "2026-10-01", Key: "a", Value: 1},
		{Date: "2026-10-03", Key: "b", Value: 5},
		{Date: "2026-10-03", Key: "a", Value: 2},
	}, "2026-10-04")
	if !slices.Equal(days, []string{"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"}) {
		t.Fatalf("days = %v", days)
	}
	if !slices.Equal(byKey["a"], []float64{1, 1, 2, 2}) {
		t.Errorf("a = %v", byKey["a"])
	}
	if b := byKey["b"]; !math.IsNaN(b[0]) || !math.IsNaN(b[1]) || b[2] != 5 || b[3] != 5 {
		t.Errorf("b = %v, want unknown until its first day", b)
	}
}

func TestComparePeriod(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse(dateLayout, s); return v }
	for _, c := range []struct {
		from, to, cmpFrom, cmpTo string
		axis                     int
		mtd                      bool
	}{
		{"2026-10-01", "2026-10-16", "2026-09-01", "2026-09-30", 31, true},
		{"2026-03-01", "2026-03-31", "2026-02-01", "2026-02-28", 31, true},
		{"2026-09-17", "2026-10-16", "2026-08-18", "2026-09-16", 30, false},
	} {
		cf, ct, axis, mtd := comparePeriod(d(c.from), d(c.to))
		if cf.Format(dateLayout) != c.cmpFrom || ct.Format(dateLayout) != c.cmpTo || axis != c.axis || mtd != c.mtd {
			t.Errorf("%s..%s: %s..%s axis %d mtd %v", c.from, c.to, cf.Format(dateLayout), ct.Format(dateLayout), axis, mtd)
		}
	}
}
