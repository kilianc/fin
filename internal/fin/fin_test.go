package fin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
)

func mutationErr() error {
	return &plaid.Error{Type: "TRANSACTIONS_ERROR", Code: "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION"}
}

func syncIDs(res *syncResult) []string {
	ids := []string{}
	for id := range res.transactions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestSyncPagesUntilHasMoreIsFalse(t *testing.T) {
	f := newFakePlaid()
	pages := map[string]*plaid.TransactionsSyncResponse{
		"":   {Added: []plaid.Transaction{tx("t1", "a", "2026-01-01", 1)}, NextCursor: "c1", HasMore: true},
		"c1": {Added: []plaid.Transaction{tx("t2", "a", "2026-01-02", 2)}, NextCursor: "c2", HasMore: true},
		"c2": {Added: []plaid.Transaction{tx("t3", "a", "2026-01-03", 3)}, NextCursor: "c3", TransactionsUpdateStatus: "HISTORICAL_UPDATE_COMPLETE"},
	}
	f.syncPages["tok"] = func(cursor string, _ int) (*plaid.TransactionsSyncResponse, error) { return pages[cursor], nil }

	res, err := syncTransactions(context.Background(), f, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := syncIDs(res); !slices.Equal(got, []string{"t1", "t2", "t3"}) {
		t.Errorf("transactions = %v", got)
	}
	if res.cursor != "c3" || res.status != "HISTORICAL_UPDATE_COMPLETE" {
		t.Errorf("cursor, status = %q, %q", res.cursor, res.status)
	}
	if len(f.calls) != 3 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestSyncAppliesModifiedAndRemoved(t *testing.T) {
	f := newFakePlaid()
	pages := map[string]*plaid.TransactionsSyncResponse{
		"": {
			Added:      []plaid.Transaction{tx("t1", "a", "2026-01-01", 10), tx("t2", "a", "2026-01-02", 20), tx("t3", "a", "2026-01-03", 30)},
			NextCursor: "c1", HasMore: true,
		},
		"c1": {
			Modified:   []plaid.Transaction{tx("t2", "a", "2026-01-02", 25)},
			Removed:    []plaid.RemovedTransaction{{TransactionID: "t3"}},
			NextCursor: "c2",
		},
	}
	f.syncPages["tok"] = func(cursor string, _ int) (*plaid.TransactionsSyncResponse, error) { return pages[cursor], nil }

	res, err := syncTransactions(context.Background(), f, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := syncIDs(res); !slices.Equal(got, []string{"t1", "t2"}) {
		t.Fatalf("transactions = %v, want t1 and t2 (t3 removed)", got)
	}
	if amt := res.transactions["t2"].Amount; amt != 25 {
		t.Errorf("t2 amount = %v, want the modified 25", amt)
	}
}

func TestSyncRestartsFromOriginalCursorAfterMutation(t *testing.T) {
	f := newFakePlaid()
	f.syncPages["tok"] = func(cursor string, call int) (*plaid.TransactionsSyncResponse, error) {
		switch {
		case cursor == "c0" && call == 0:
			// First pass: a transaction that changes before pagination ends.
			return &plaid.TransactionsSyncResponse{Added: []plaid.Transaction{tx("stale", "a", "2026-01-01", 1)}, NextCursor: "c1", HasMore: true}, nil
		case cursor == "c1" && call == 1:
			return nil, mutationErr()
		case cursor == "c0":
			return &plaid.TransactionsSyncResponse{Added: []plaid.Transaction{tx("t1", "a", "2026-01-01", 1)}, NextCursor: "c1", HasMore: true}, nil
		case cursor == "c1":
			return &plaid.TransactionsSyncResponse{Added: []plaid.Transaction{tx("t2", "a", "2026-01-02", 2)}, NextCursor: "c2"}, nil
		}
		t.Fatalf("unexpected cursor %q on call %d", cursor, call)
		return nil, nil
	}

	res, err := syncTransactions(context.Background(), f, "tok", "c0")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`sync tok cursor="c0"`, `sync tok cursor="c1"`, `sync tok cursor="c0"`, `sync tok cursor="c1"`}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v\nwant restart from the original cursor c0: %v", f.calls, want)
	}
	if got := syncIDs(res); !slices.Equal(got, []string{"t1", "t2"}) {
		t.Errorf("transactions = %v, want pages from the aborted pass discarded", got)
	}
	if res.cursor != "c2" {
		t.Errorf("cursor = %q", res.cursor)
	}
}

func TestSyncGivesUpAfterRepeatedMutations(t *testing.T) {
	f := newFakePlaid()
	f.syncPages["tok"] = func(string, int) (*plaid.TransactionsSyncResponse, error) { return nil, mutationErr() }

	_, err := syncTransactions(context.Background(), f, "tok", "")
	if !plaid.IsCode(err, "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != maxSyncRestarts+1 {
		t.Errorf("calls = %d, want %d", len(f.calls), maxSyncRestarts+1)
	}
}

func TestAccountsIsolatesItemFailures(t *testing.T) {
	ta := newTestApp(t, chase, citi)
	ta.fake.errs["tok-item-chase"] = &plaid.Error{Type: "ITEM_ERROR", Code: "ITEM_LOGIN_REQUIRED", Message: "login required"}
	ta.fake.accounts["tok-item-citi"] = []plaid.Account{{AccountID: "citi-1", Name: "Double Cash", Mask: ptr("1234"), Type: "credit"}}

	code, body := ta.run(t, "accounts")
	if code != exitPartial {
		t.Fatalf("exit = %d, want %d; stderr %s", code, exitPartial, ta.stderr)
	}
	accounts := body["accounts"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["item"] != "citi" {
		t.Errorf("accounts = %v, want citi's account only", accounts)
	}
	errs := body["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v", errs)
	}
	e := errs[0].(map[string]any)
	if e["item"] != "chase" || e["code"] != "ITEM_LOGIN_REQUIRED" || e["action"] != "run fin reconnect chase" {
		t.Errorf("error = %v", e)
	}
	if !strings.Contains(e["message"].(string), "fin reconnect chase") {
		t.Errorf("message %q does not tell the user what to run", e["message"])
	}
}

func TestAccountsSucceedsWithExitZero(t *testing.T) {
	ta := newTestApp(t, citi)
	ta.fake.accounts["tok-item-citi"] = []plaid.Account{{AccountID: "citi-1", Name: "Double Cash"}}
	code, body := ta.run(t, "accounts")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	if errs := body["errors"].([]any); len(errs) != 0 {
		t.Errorf("errors = %v, want an empty array", errs)
	}
}

func TestTransactionsFiltersAndRecordsSync(t *testing.T) {
	ta := newTestApp(t, chase, citi, fido)
	ta.fake.syncPages["tok-item-chase"] = func(cursor string, _ int) (*plaid.TransactionsSyncResponse, error) {
		return &plaid.TransactionsSyncResponse{
			Added: []plaid.Transaction{
				tx("old", "chk", "2025-12-31", 5),
				tx("in1", "chk", "2026-01-15", 12.5),
				tx("in2", "card", "2026-02-01", 40),
				tx("late", "chk", "2026-03-01", 7),
			},
			Accounts: []plaid.Account{
				{AccountID: "chk", Name: "Checking", Mask: ptr("0001")},
				{AccountID: "card", Name: "Sapphire", Mask: ptr("9999")},
			},
			NextCursor:               "chase-c1",
			TransactionsUpdateStatus: "HISTORICAL_UPDATE_COMPLETE",
		}, nil
	}
	ta.fake.syncPages["tok-item-citi"] = func(string, int) (*plaid.TransactionsSyncResponse, error) {
		return &plaid.TransactionsSyncResponse{TransactionsUpdateStatus: "NOT_READY"}, nil
	}

	code, body := ta.run(t, "transactions", "--since", "2026-01-01", "--until", "2026-02-28")
	if code != exitPartial {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	txs := body["transactions"].([]any)
	var ids []string
	for _, v := range txs {
		ids = append(ids, v.(map[string]any)["transaction_id"].(string))
	}
	if !slices.Equal(ids, []string{"in2", "in1"}) {
		t.Errorf("ids = %v, want in-range transactions, newest first", ids)
	}
	if name := txs[0].(map[string]any)["account_name"]; name != "Sapphire" {
		t.Errorf("account_name = %v", name)
	}
	errs := body["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["code"] != "TRANSACTIONS_NOT_READY" {
		t.Errorf("errors = %v", errs)
	}
	for _, c := range ta.fake.calls {
		if strings.Contains(c, "item-fido") {
			t.Errorf("brokerage Item was asked for transactions: %s", c)
		}
	}

	st, err := state.Load(ta.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Find("sandbox", "chase")
	if got.TransactionsCursor != "chase-c1" || got.LastSync == nil || !got.LastSync.Equal(testNow) {
		t.Errorf("chase state = %+v", got)
	}
	if c, _ := st.Find("sandbox", "citi"); c.LastSync != nil {
		t.Errorf("citi recorded a sync although it failed: %+v", c)
	}

	code, body = ta.run(t, "transactions", "--account", "9999", "--since", "2026-01-01")
	if code != exitPartial {
		t.Fatalf("exit = %d", code)
	}
	if txs := body["transactions"].([]any); len(txs) != 1 || txs[0].(map[string]any)["transaction_id"] != "in2" {
		t.Errorf("--account by mask: %v", txs)
	}
}

func TestTransactionsRequiresSince(t *testing.T) {
	ta := newTestApp(t, chase)
	code, _ := ta.run(t, "transactions")
	if code != exitUsage || ta.stderrJSON(t)["code"] != "USAGE" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
}

func TestHoldingsJoinsSecuritiesAndKeepsTaxLots(t *testing.T) {
	ta := newTestApp(t, fido)
	ta.fake.holdings["tok-item-fido"] = &plaid.HoldingsResponse{
		Accounts:   []plaid.Account{{AccountID: "ira", Name: "Roth IRA"}},
		Securities: []plaid.Security{{SecurityID: "s-vti", TickerSymbol: ptr("VTI"), Name: ptr("Vanguard Total Stock Market ETF")}},
		Holdings: []plaid.Holding{{
			AccountID: "ira", SecurityID: "s-vti", Quantity: 10, InstitutionPrice: 300, InstitutionValue: 3000, CostBasis: ptr(2500.0),
			TaxLots: []plaid.TaxLot{{Quantity: ptr(10.0), CostBasis: ptr(2500.0), OriginalPurchaseDatetime: ptr("2024-03-01T00:00:00Z")}},
		}},
	}
	code, body := ta.run(t, "holdings")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	h := body["holdings"].([]any)[0].(map[string]any)
	if h["ticker"] != "VTI" || h["account_name"] != "Roth IRA" || h["cost_basis"] != 2500.0 {
		t.Errorf("holding = %v", h)
	}
	if lots := h["tax_lots"].([]any); len(lots) != 1 {
		t.Errorf("tax_lots = %v", lots)
	}
}

func TestInvestmentsPagesByOffset(t *testing.T) {
	ta := newTestApp(t, fido)
	ta.fake.invMaxPage = 2
	for i, d := range []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04", "2026-01-05"} {
		ta.fake.invTxs["tok-item-fido"] = append(ta.fake.invTxs["tok-item-fido"], plaid.InvestmentTransaction{
			InvestmentTransactionID: string(rune('a' + i)), AccountID: "acc-ira", Date: d, Type: "buy",
		})
	}
	code, body := ta.run(t, "investments", "--since", "2026-01-01")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	if n := len(body["investment_transactions"].([]any)); n != 5 {
		t.Errorf("got %d investment transactions, want 5", n)
	}
	want := []string{"inv_txs tok-item-fido offset=0", "inv_txs tok-item-fido offset=2", "inv_txs tok-item-fido offset=4"}
	if !slices.Equal(ta.fake.calls, want) {
		t.Errorf("calls = %v, want %v", ta.fake.calls, want)
	}
}

func TestItemsReportsHealthAndSlots(t *testing.T) {
	ta := newTestApp(t, chase, citi)
	ta.fake.itemErrors["tok-item-chase"] = &plaid.Error{Code: "ITEM_LOGIN_REQUIRED", Message: "login required"}
	code, body := ta.run(t, "items")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	if body["slots_used"] != 2.0 || body["slots_total"] != 10.0 {
		t.Errorf("slots = %v of %v", body["slots_used"], body["slots_total"])
	}
	items := body["items"].([]any)
	c := items[0].(map[string]any)
	if c["health"] != "needs_reconnect" || c["action"] != "run fin reconnect chase" {
		t.Errorf("chase = %v", c)
	}
	if items[1].(map[string]any)["health"] != "ok" {
		t.Errorf("citi = %v", items[1])
	}
}

func TestReadsWithNoItemsNeedNoCredentials(t *testing.T) {
	ta := newTestApp(t)
	ta.secrets.m = map[string]string{}
	code, body := ta.run(t, "accounts")
	if code != exitOK || len(body["accounts"].([]any)) != 0 {
		t.Fatalf("exit = %d, body %v, stderr %s", code, body, ta.stderr)
	}
}

func finishedLink(publicToken string) *plaid.LinkTokenGetResponse {
	now := time.Now()
	return &plaid.LinkTokenGetResponse{LinkSessions: []plaid.LinkSession{{
		FinishedAt: &now,
		Results: &plaid.LinkResults{ItemAddResults: []plaid.ItemAddResult{{
			PublicToken: publicToken,
			Institution: &plaid.Institution{InstitutionID: "ins_3", Name: "American Express"},
		}}},
	}}}
}

func TestLinkPollsThenStoresItem(t *testing.T) {
	ta := newTestApp(t, chase)
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{}, {}, finishedLink("public-1")}
	ta.fake.exchanges["public-1"] = &plaid.ExchangeResponse{AccessToken: "access-amex", ItemID: "item-amex"}

	code, body := ta.run(t, "link", "bank")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	if !strings.Contains(ta.stderr.String(), "https://hosted.plaid.com/link/abc") {
		t.Errorf("stderr does not show the Hosted Link URL: %s", ta.stderr)
	}
	req := ta.fake.linkCreate
	if !slices.Equal(req.Products, []string{"transactions"}) || req.Transactions.DaysRequested != 730 || req.HostedLink == nil || req.AccessToken != "" || slices.Contains(req.Products, "auth") {
		t.Errorf("link token request = %+v", req)
	}
	linked := body["linked"].([]any)[0].(map[string]any)
	if linked["name"] != "american-express" || body["slots_used"] != 2.0 {
		t.Errorf("body = %v", body)
	}
	if tok, _ := ta.secrets.Get(tokenAccount(plaid.Sandbox, "item-amex")); tok != "access-amex" {
		t.Errorf("access token not stored in the Keychain")
	}
	st, _ := state.Load(ta.StatePath)
	it, ok := st.Find("sandbox", "american-express")
	if !ok || it.Kind != "bank" || it.ItemID != "item-amex" {
		t.Errorf("state item = %+v", it)
	}
	if strings.Contains(ta.stdout.String(), "access-amex") {
		t.Error("access token leaked to stdout")
	}
}

func TestLinkAsksForBothProductsSoNoRelinkIsNeeded(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		required    string
		ifSupported string
	}{
		{[]string{"link"}, "transactions", "investments"},
		{[]string{"link", "bank"}, "transactions", "investments"},
		{[]string{"link", "brokerage"}, "investments", "transactions"},
	} {
		ta := newTestApp(t)
		ta.fake.linkGets = []*plaid.LinkTokenGetResponse{finishedLink("public-1")}
		ta.fake.exchanges["public-1"] = &plaid.ExchangeResponse{AccessToken: "access-1", ItemID: "item-1"}
		ta.fake.itemProducts["access-1"] = []string{"assets", "transactions", "investments"}
		if code, _ := ta.run(t, tc.args...); code != exitOK {
			t.Fatalf("%v: exit = %d; stderr %s", tc.args, code, ta.stderr)
		}
		req := ta.fake.linkCreate
		if !slices.Equal(req.Products, []string{tc.required}) || !slices.Equal(req.RequiredIfSupportedProducts, []string{tc.ifSupported}) {
			t.Errorf("%v: products %v, if supported %v", tc.args, req.Products, req.RequiredIfSupportedProducts)
		}
		st, _ := state.Load(ta.StatePath)
		if it := st.Items[0]; !slices.Equal(it.Products, []string{"investments", "transactions"}) {
			t.Errorf("%v: recorded products %v, want what /item/get reports, minus products fin does not read", tc.args, it.Products)
		}
	}
}

func TestLinkInProductionNeedsConfirmation(t *testing.T) {
	ta := newTestApp(t)
	ta.Env = plaid.Production
	ta.secrets.m[secretAccount(plaid.Production)] = "secret"

	code, _ := ta.run(t, "link", "bank")
	if code != exitError || ta.stderrJSON(t)["code"] != "CONFIRMATION_REQUIRED" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
	if len(ta.fake.calls) != 0 {
		t.Errorf("Plaid was called before confirmation: %v", ta.fake.calls)
	}

	ta.tty = true
	ta.Stdin = strings.NewReader("n\n")
	if code, _ := ta.run(t, "link", "bank"); code != exitError || ta.stderrJSON(t)["code"] != "ABORTED" {
		t.Fatalf("answering no: exit = %d, stderr %s", code, ta.stderr)
	}
	if !strings.Contains(ta.stderr.String(), "slot 1 of 10") {
		t.Errorf("prompt does not show slots: %s", ta.stderr)
	}
}

func TestLinkInProductionStopsWhenSlotsAreFull(t *testing.T) {
	var items []state.Item
	for i := range SlotsTotal {
		items = append(items, state.Item{Name: string(rune('a' + i)), ItemID: string(rune('a' + i)), Env: "production"})
	}
	ta := newTestApp(t, items...)
	ta.Env = plaid.Production
	code, _ := ta.run(t, "link", "bank", "--yes")
	if code != exitError || ta.stderrJSON(t)["code"] != "NO_SLOTS" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
}

func TestLinkReportsExitWithoutItem(t *testing.T) {
	ta := newTestApp(t)
	now := time.Now()
	exit := &plaid.LinkExit{}
	exit.Metadata.Status = "institution_not_found"
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{LinkSessions: []plaid.LinkSession{{FinishedAt: &now, Exit: exit}}}}
	code, _ := ta.run(t, "link", "bank")
	if code != exitError || ta.stderrJSON(t)["code"] != "LINK_EXITED" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
}

func TestReconnectUsesUpdateMode(t *testing.T) {
	ta := newTestApp(t, chase)
	now := time.Now()
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{}, {LinkSessions: []plaid.LinkSession{{FinishedAt: &now}}}}

	code, body := ta.run(t, "reconnect", "chase")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	req := ta.fake.linkCreate
	if req.AccessToken != "tok-item-chase" || len(req.Products) != 0 {
		t.Errorf("update-mode request = %+v", req)
	}
	if body["reconnected"].(map[string]any)["health"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestReconnectFailsWhenItemStillBroken(t *testing.T) {
	ta := newTestApp(t, chase)
	now := time.Now()
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{LinkSessions: []plaid.LinkSession{{FinishedAt: &now}}}}
	ta.fake.itemErrors["tok-item-chase"] = &plaid.Error{Code: "ITEM_LOGIN_REQUIRED"}
	code, _ := ta.run(t, "reconnect", "chase")
	if code != exitError || ta.stderrJSON(t)["code"] != "RECONNECT_INCOMPLETE" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
}

func TestReconnectUnknownItem(t *testing.T) {
	ta := newTestApp(t, chase)
	code, _ := ta.run(t, "reconnect", "amex")
	if code != exitError || ta.stderrJSON(t)["code"] != "ITEM_NOT_FOUND" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
}

func TestTableOutput(t *testing.T) {
	ta := newTestApp(t, citi)
	ta.fake.accounts["tok-item-citi"] = []plaid.Account{{AccountID: "c", Name: "Double Cash", Type: "credit", Balances: plaid.Balances{Current: ptr(12.5)}}}
	ta.stdout.Reset()
	if code := ta.Run(context.Background(), []string{"accounts", "--table"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	out := ta.stdout.String()
	if !strings.HasPrefix(out, "Accounts") || !strings.Contains(out, "Double Cash") || !strings.Contains(out, "12.50") || strings.Contains(out, "{") {
		t.Errorf("table = %q", out)
	}
}

func TestEnvSwitchIsSavedAndUsed(t *testing.T) {
	ta := newTestApp(t)
	ta.Env = ""
	code, body := ta.run(t, "env")
	if code != exitOK || body["env"] != "sandbox" || body["source"] != "default" {
		t.Fatalf("exit = %d, body = %v", code, body)
	}

	ta.Env = ""
	if code, body = ta.run(t, "env", "production"); code != exitOK || body["env"] != "production" {
		t.Fatalf("exit = %d, body = %v", code, body)
	}

	ta.Env = ""
	if code, body = ta.run(t, "env"); body["env"] != "production" || body["source"] != "fin env" {
		t.Fatalf("saved env not used: %v", body)
	}

	ta.Env, ta.EnvVar = "", "sandbox"
	if code, body = ta.run(t, "env"); body["env"] != "sandbox" || body["source"] != "PLAID_ENV" {
		t.Fatalf("PLAID_ENV does not override: %v", body)
	}

	ta.Env, ta.EnvVar = "", ""
	if code, _ = ta.run(t, "env", "prod"); code != exitUsage {
		t.Fatalf("bad env accepted: exit %d", code)
	}
}

func TestReconnectAddsProduct(t *testing.T) {
	schwab := state.Item{Name: "charles-schwab", ItemID: "item-schwab", Kind: "bank", Products: []string{"transactions"}, InstitutionName: "Charles Schwab"}
	ta := newTestApp(t, schwab)
	now := time.Now()
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{LinkSessions: []plaid.LinkSession{{FinishedAt: &now}}}}
	ta.fake.holdings["tok-item-schwab"] = &plaid.HoldingsResponse{}

	code, _ := ta.run(t, "reconnect", "charles-schwab", "--add", "investments")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	req := ta.fake.linkCreate
	if !slices.Equal(req.AdditionalConsentedProducts, []string{"investments"}) || len(req.Products) != 0 || req.AccessToken != "tok-item-schwab" {
		t.Errorf("update-mode request = %+v", req)
	}
	st, _ := state.Load(ta.StatePath)
	it, _ := st.Find("sandbox", "charles-schwab")
	if !slices.Equal(it.Products, []string{"transactions", "investments"}) {
		t.Errorf("products = %v", it.Products)
	}
}

func TestReconnectAddFailsWithoutConsent(t *testing.T) {
	schwab := state.Item{Name: "charles-schwab", ItemID: "item-schwab", Kind: "bank", Products: []string{"transactions"}, InstitutionName: "Charles Schwab"}
	ta := newTestApp(t, schwab)
	now := time.Now()
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{LinkSessions: []plaid.LinkSession{{FinishedAt: &now}}}}
	ta.fake.holdingsErr = &plaid.Error{Code: "ADDITIONAL_CONSENT_REQUIRED"}

	code, _ := ta.run(t, "reconnect", "charles-schwab", "--add", "investments")
	if code != exitError || ta.stderrJSON(t)["code"] != "PRODUCT_NOT_ADDED" {
		t.Fatalf("exit = %d, stderr %s", code, ta.stderr)
	}
	st, _ := state.Load(ta.StatePath)
	if it, _ := st.Find("sandbox", "charles-schwab"); it.HasProduct("investments") {
		t.Error("investments recorded although consent was not granted")
	}
}

func TestReconnectFinishesOnHandoffWithoutFinishedAt(t *testing.T) {
	ta := newTestApp(t, chase)
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{
		{LinkSessions: []plaid.LinkSession{{Events: []plaid.LinkEvent{{EventName: "OPEN"}}}}},
		{LinkSessions: []plaid.LinkSession{{Events: []plaid.LinkEvent{{EventName: "OPEN"}, {EventName: "HANDOFF"}}}}},
	}
	if code, _ := ta.run(t, "reconnect", "chase"); code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
}

func TestLinkUsesLegacyOnSuccessToken(t *testing.T) {
	ta := newTestApp(t)
	ta.fake.linkGets = []*plaid.LinkTokenGetResponse{{LinkSessions: []plaid.LinkSession{{OnSuccess: &plaid.LinkOnSuccess{PublicToken: "public-legacy"}}}}}
	ta.fake.exchanges["public-legacy"] = &plaid.ExchangeResponse{AccessToken: "a", ItemID: "i"}
	if code, _ := ta.run(t, "link", "bank"); code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
}

func TestInitPrintsTaglineAndAgentPrompt(t *testing.T) {
	ta := newTestApp(t)
	code, body := ta.run(t, "init")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr %s", code, ta.stderr)
	}
	if body["tagline"] != Tagline || !strings.Contains(body["prompt"].(string), "dashboard.plaid.com/trial-plan") {
		t.Errorf("body = %v", body)
	}
	status := body["status"].(map[string]any)
	if status["credentials"].(map[string]any)["sandbox"] != true || status["credentials"].(map[string]any)["production"] != false {
		t.Errorf("status = %v", status)
	}
	if !strings.Contains(status["next"].(string), "Trial plan") {
		t.Errorf("next = %v", status["next"])
	}
}

func TestHumanOutputWhenStdoutIsATerminal(t *testing.T) {
	ta := newTestApp(t)
	ta.StdoutIsTerminal = func() bool { return true }
	ta.stdout.Reset()
	if code := ta.Run(context.Background(), []string{"init"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	out := ta.stdout.String()
	if strings.HasPrefix(out, "{") || !strings.Contains(out, "you own all of it end to end") {
		t.Errorf("init output = %q", out)
	}
	ta.stdout.Reset()
	ta.Run(context.Background(), []string{"init", "--json"})
	if !strings.HasPrefix(ta.stdout.String(), "{") {
		t.Errorf("--json did not force JSON: %q", ta.stdout.String())
	}
}

func TestTablesHideCurrencyWhenAllUSD(t *testing.T) {
	ta := newTestApp(t, citi)
	ta.fake.accounts["tok-item-citi"] = []plaid.Account{{AccountID: "c", Name: "Double Cash", Type: "credit", Balances: plaid.Balances{Current: ptr(12.5), IsoCurrencyCode: ptr("USD")}}}
	ta.stdout.Reset()
	ta.Run(context.Background(), []string{"accounts", "--table"})
	if strings.Contains(ta.stdout.String(), "Ccy") {
		t.Errorf("USD-only table shows a currency column:\n%s", ta.stdout)
	}
	ta.fake.accounts["tok-item-citi"][0].Balances.IsoCurrencyCode = ptr("CAD")
	ta.stdout.Reset()
	ta.Run(context.Background(), []string{"accounts", "--table"})
	if !strings.Contains(ta.stdout.String(), "CAD") {
		t.Errorf("non-USD table hides the currency:\n%s", ta.stdout)
	}
}

var update = flag.Bool("update", false, "rewrite docs/ from the agent prompt")

func init() {
	b, err := os.ReadFile("../../docs/site.css")
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	SiteCSSVersion = hex.EncodeToString(sum[:])[:10]
}

// The README's buttons open docs/<agent>/index.html on GitHub Pages, which
// carries the agent prompt. Regenerate with: make docs
func TestSetupPagesMatchThePrompt(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(shortPrompt) > 500 {
		t.Errorf("short prompt is %d characters; Claude Code stalls typing long deep-link prompts", len(shortPrompt))
	}
	for _, agent := range Agents() {
		if !strings.Contains(string(readme), `href="`+PagesURL+agent.Slug+`/"`) {
			t.Errorf("README has no button linking to %s%s/", PagesURL, agent.Slug)
		}
		checkGenerated(t, "../../docs/"+agent.Slug+"/index.html", SetupPage(agent))
	}
}

// The gallery's try pages carry each example prompt into Claude Code and
// Codex. Regenerate with: make docs
func TestTryPagesMatchTheExamples(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"README.md", "docs/index.html", "docs/examples/index.html"} {
		data, err := os.ReadFile("../../" + name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(data)
	}
	for _, e := range Examples {
		if n := len(e.agentPrompt()); n > 500 {
			t.Errorf("%s prompt is %d characters; Claude Code stalls typing long deep-link prompts", e.Slug, n)
		}
		if !strings.Contains(files["README.md"], `href="`+e.TryURL()+`"`) {
			t.Errorf("README has no try link for %s", e.Slug)
		}
		if !strings.Contains(files["docs/index.html"], `href="try/`+e.Slug+`/"`) {
			t.Errorf("docs/index.html has no try link for %s", e.Slug)
		}
		if !strings.Contains(files["docs/examples/index.html"], "“"+e.Ask+"”") || !strings.Contains(files["docs/examples/index.html"], e.Detail) {
			t.Errorf("docs/examples/index.html doesn't show the %s prompt verbatim", e.Slug)
		}
		checkGenerated(t, "../../docs/try/"+e.Slug+"/index.html", TryPage(e))
	}
}

func checkGenerated(t *testing.T, path, want string) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Errorf("%s is out of date; regenerate it with: make docs", path)
	}
}

// TestSitePagesShareOneStyle keeps the landing page, setup pages and try
// pages on the same head, header and footer, with every style in
// docs/site.css.
func TestSitePagesShareOneStyle(t *testing.T) {
	pages := map[string]string{"../../docs/index.html": "./"}
	for _, a := range Agents() {
		pages["../../docs/"+a.Slug+"/index.html"] = "../"
	}
	for _, e := range Examples {
		pages["../../docs/try/"+e.Slug+"/index.html"] = "../../"
	}
	for path, root := range pages {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		page := string(b)
		if *update && root == "./" {
			// the landing page is hand-written; keep its stylesheet link current
			page = regexp.MustCompile(`href="\./site\.css[^"]*"`).ReplaceAllString(page, `href="./site.css?v=`+SiteCSSVersion+`"`)
			if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, part := range []string{"head", "header", "footer"} {
			if !strings.Contains(page, sitePart(part, root)) {
				t.Errorf("%s doesn't carry the shared %s", path, part)
			}
		}
		if root == "./" && !strings.Contains(page, `<template id="setup-prompt">`+template.HTMLEscapeString(agentPrompt)+`</template>`) {
			t.Errorf("%s doesn't copy the current setup prompt", path)
		}
		if strings.Contains(page, "<style") || strings.Contains(page, ` style="`) {
			t.Errorf("%s has styles of its own; put them in docs/site.css", path)
		}
	}
}

// TestSkillShipsAsAPlugin keeps the skill, the plugin manifests, the Codex
// symlink and the install lines that fin init and the README print in step.
func TestSkillShipsAsAPlugin(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile("../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	skill := read("skills/fin/SKILL.md")
	if !strings.HasPrefix(skill, "---\nname: fin\ndescription: ") {
		t.Error("skills/fin/SKILL.md needs frontmatter starting with name: fin and a description")
	}
	if codex := read(".agents/skills/fin/SKILL.md"); codex != skill {
		t.Error(".agents/skills/fin should link to skills/fin for Codex")
	}
	var plugin struct{ Name string }
	var market struct {
		Name    string
		Plugins []struct{ Name, Source string }
	}
	if err := json.Unmarshal([]byte(read(".claude-plugin/plugin.json")), &plugin); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(read(".claude-plugin/marketplace.json")), &market); err != nil {
		t.Fatal(err)
	}
	if len(market.Plugins) != 1 || market.Plugins[0].Name != plugin.Name || market.Plugins[0].Source != "./" {
		t.Errorf("marketplace should list the repo root as plugin %q, got %+v", plugin.Name, market.Plugins)
	}
	install := "/plugin install " + plugin.Name + "@" + market.Name
	if !slices.Contains(SkillInstall["claude"], install) {
		t.Errorf("fin init should print %q", install)
	}
	readme := read("README.md")
	for _, line := range append(SkillInstall["claude"], SkillInstall["codex"]...) {
		if !strings.Contains(readme, line) {
			t.Errorf("README is missing %q", line)
		}
	}
}
