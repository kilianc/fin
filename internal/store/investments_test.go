package store

import (
	"context"
	"testing"

	"github.com/kilianc/fin/internal/plaid"
)

var hoodAccount = InvestmentAccount{AccountID: "acc-ind", ItemID: "item-hood", Item: "robinhood", Institution: "Robinhood", Name: "Robinhood individual"}

func hoodSync(t *testing.T, s *Store, from string, txs ...plaid.InvestmentTransaction) {
	t.Helper()
	if err := s.ApplyInvestments(context.Background(), InvestmentSync{ItemID: "item-hood", Item: "robinhood", Institution: "Robinhood",
		Day: "2026-10-09", SyncedAt: syncedAt, Accounts: []plaid.Account{{AccountID: "acc-ind", Name: "Robinhood individual", Type: "investment"}},
		From: from, Txs: txs}); err != nil {
		t.Fatal(err)
	}
}

// visible maps each investment transaction fin reads to its source.
func visible(t *testing.T, s *Store) map[string]string {
	t.Helper()
	txs, err := s.InvestmentTransactions(context.Background(), Filter{ItemIDs: []string{"item-hood"}, From: "2000-01-01", To: "2099-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, tx := range txs {
		out[tx.InvestmentTransactionID] = tx.Source
	}
	return out
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func same(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v; want %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v; want %v", what, got, want)
		}
	}
}

// oldStore rewinds a new store to the single investment_transactions table
// fin kept before the raw tables, with or without its source column.
func oldStore(t *testing.T, withSource bool) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	s, path := openTemp(t)
	hoodSync(t, s, "2026-09-01")
	for _, q := range []string{
		`drop view investment_transactions`, `drop view investment_matches`,
		`drop table plaid_investment_transactions`, `drop table robinhood_activity`, `drop table robinhood_reports`,
		`create table investment_transactions (investment_transaction_id varchar primary key, item_id varchar not null,
			account_id varchar not null, security_id varchar, date date not null, name varchar not null, type varchar not null,
			subtype varchar, quantity double not null, price double not null, amount double not null, fees double, iso_currency_code varchar)`,
		`insert into investment_transactions values ('p-old', 'item-hood', 'acc-ind', null, '2026-09-02', 'Interest', 'cash', 'interest', 0, 0, -1, null, 'USD')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if withSource {
		for _, q := range []string{
			`alter table investment_transactions add column source varchar default 'plaid'`,
			`insert into investment_transactions values ('robinhood-a', 'item-hood', 'acc-ind', null, '2018-11-12', 'Apex to RHS Conversion · CONV', 'transfer', 'transfer', 0, 0, -148.96, null, 'USD', 'robinhood')`,
		} {
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOldInvestmentTransactionsMoveToRawTables(t *testing.T) {
	s, _ := oldStore(t, false)
	same(t, "from a file without the source column", visible(t, s), map[string]string{"p-old": "plaid"})

	s, path := oldStore(t, true)
	same(t, "from a file with imported rows", visible(t, s), map[string]string{"p-old": "plaid", "robinhood-a": "robinhood"})
	if n := count(t, s, `select count(*) from robinhood_activity where key = 'robinhood-a'`); n != 1 {
		t.Errorf("imported row in robinhood_activity = %d", n)
	}
	// It syncs, and opening it again changes nothing.
	hoodSync(t, s, "2026-10-01", plaid.InvestmentTransaction{InvestmentTransactionID: "p-new", AccountID: "acc-ind", Date: "2026-10-02", Name: "Dividend", Type: "cash", Amount: -2})
	s.Close()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	same(t, "after a sync and reopening", visible(t, s), map[string]string{"p-old": "plaid", "p-new": "plaid", "robinhood-a": "robinhood"})
}

func TestPlaidRowsPlaidStopsSendingAreKept(t *testing.T) {
	s, _ := openTemp(t)
	a := plaid.InvestmentTransaction{InvestmentTransactionID: "a", AccountID: "acc-ind", Date: "2026-09-10", Name: "Buy", Type: "buy", Amount: 10}
	b := plaid.InvestmentTransaction{InvestmentTransactionID: "b", AccountID: "acc-ind", Date: "2026-09-11", Name: "Buy", Type: "buy", Amount: 20}
	hoodSync(t, s, "2026-09-01", a, b)
	hoodSync(t, s, "2026-09-01", a)
	same(t, "after Plaid dropped b", visible(t, s), map[string]string{"a": "plaid"})
	if n := count(t, s, `select count(*) from plaid_investment_transactions where investment_transaction_id = 'b' and removed_at is not null`); n != 1 {
		t.Errorf("b kept and marked removed = %d", n)
	}
	hoodSync(t, s, "2026-09-01", a, b)
	same(t, "after b came back", visible(t, s), map[string]string{"a": "plaid", "b": "plaid"})
}

func TestReportsSpeakForTheirDates(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	hoodSync(t, s, "2026-09-01",
		plaid.InvestmentTransaction{InvestmentTransactionID: "p-in", AccountID: "acc-ind", Date: "2026-09-15", Name: "Interest", Type: "cash", Amount: -463.09},
		// A trade on the report's last day, dated a day later by Plaid.
		plaid.InvestmentTransaction{InvestmentTransactionID: "p-edge", AccountID: "acc-ind", Date: "2026-10-01", Name: "Buy", Type: "buy", Amount: 600},
		plaid.InvestmentTransaction{InvestmentTransactionID: "p-after", AccountID: "acc-ind", Date: "2026-10-03", Name: "Buy", Type: "buy", Amount: 75},
	)
	rows := []RobinhoodRow{
		{Key: "robinhood-1", ActivityDate: "2026-09-01", Name: "Gold Deposit Boost Payment · GDBP", Type: "cash", Amount: -178.28},
		{Key: "robinhood-2", ActivityDate: "2026-09-15", Name: "Interest Payment · INT", Type: "cash", Amount: -435.07},
		{Key: "robinhood-3", ActivityDate: "2026-09-30", Name: "SpaceX · Buy", Type: "buy", Amount: 600},
	}
	reports := []RobinhoodReport{{ID: "r1", File: "a.csv", Rows: 3, First: "2026-09-01", Last: "2026-09-30"}}
	res, err := s.ImportRobinhood(ctx, hoodAccount, reports, rows, nil, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if res.New != 3 || res.Known != 0 || res.PlaidReplaced != 2 {
		t.Errorf("import = %+v", res)
	}
	same(t, "read", visible(t, s), map[string]string{"robinhood-1": "robinhood", "robinhood-2": "robinhood", "robinhood-3": "robinhood", "p-after": "plaid"})
	if n := count(t, s, `select count(*) from investment_matches where plaid_id = 'p-edge' and robinhood_key = 'robinhood-3'`); n != 1 {
		t.Errorf("p-edge not matched to the report's trade")
	}
	if n := count(t, s, `select count(*) from plaid_investment_transactions`); n != 3 {
		t.Errorf("Plaid rows stored = %d; every one is kept", n)
	}
	// A smaller report later removes nothing.
	res, err = s.ImportRobinhood(ctx, hoodAccount, []RobinhoodReport{{ID: "r2", File: "b.csv", Rows: 1, First: "2026-09-01", Last: "2026-09-01"}}, rows[:1], nil, syncedAt)
	if err != nil || res.New != 0 || res.Known != 1 {
		t.Errorf("second import = %+v, %v", res, err)
	}
	if n := count(t, s, `select count(*) from robinhood_activity`); n != 3 {
		t.Errorf("report rows = %d after a smaller report; want 3", n)
	}
}
