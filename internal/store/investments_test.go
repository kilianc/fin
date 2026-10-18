package store

import (
	"context"
	"testing"

	"github.com/kilianc/fin/internal/plaid"
)

// A file written by a fin before imports has no source column; one written
// after has Robinhood rows next to Plaid's. Both must keep syncing, and a
// Plaid sync must never touch imported rows, even inside its window.
func TestInvestmentSyncKeepsImportedRows(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	hood := InvestmentAccount{AccountID: "acc-ind", ItemID: "item-hood", Item: "robinhood", Institution: "Robinhood", Name: "Robinhood individual"}
	plaidSync := func(s *Store, from string, txs ...plaid.InvestmentTransaction) {
		t.Helper()
		if err := s.ApplyInvestments(ctx, InvestmentSync{ItemID: "item-hood", Item: "robinhood", Institution: "Robinhood", Day: "2026-10-09", SyncedAt: syncedAt,
			Accounts: []plaid.Account{{AccountID: "acc-ind", Name: "Robinhood individual", Type: "investment"}}, From: from, Txs: txs}); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(s *Store) map[string]string {
		t.Helper()
		txs, err := s.InvestmentTransactions(ctx, Filter{ItemIDs: []string{"item-hood"}, From: "2000-01-01", To: "2099-12-31"})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, tx := range txs {
			out[tx.InvestmentTransactionID] = tx.Source
		}
		return out
	}

	// The file as fin 0.5.0 left it: no source column, one Plaid row.
	plaidSync(s, "2026-09-01")
	if _, err := s.db.ExecContext(ctx, `alter table investment_transactions drop column source`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `insert into investment_transactions values ('old', 'item-hood', 'acc-ind', null, '2026-09-01', 'Interest', 'cash', 'interest', 0, 0, -1, null, 'USD')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(ctx, path); err != nil {
		t.Fatal(err)
	} else {
		if got := rows(s); got["old"] != "plaid" {
			t.Errorf("a row from before the source column = %v; want plaid", got)
		}
		s.Close()
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// An import with a row inside the window Plaid will re-read below.
	imported := []plaid.InvestmentTransaction{
		{InvestmentTransactionID: "robinhood-a", AccountID: "acc-ind", Date: "2018-11-12", Name: "Apex to RHS Conversion", Type: "transfer", Amount: -148.96},
		{InvestmentTransactionID: "robinhood-b", AccountID: "acc-ind", Date: "2026-08-31", Name: "Gold Deposit Boost Payment", Type: "cash", Amount: -134.61},
	}
	if _, err := s.ImportInvestments(ctx, hood, "robinhood", imported, nil, syncedAt); err != nil {
		t.Fatal(err)
	}
	if from, err := s.InvestmentsFrom(ctx, "item-hood"); err != nil || from != "2026-09-01" {
		t.Errorf("InvestmentsFrom = %q, %v; imported rows must not move Plaid's window", from, err)
	}
	plaidSync(s, "2026-08-01", plaid.InvestmentTransaction{InvestmentTransactionID: "new", AccountID: "acc-ind", Date: "2026-10-01", Name: "Dividend", Type: "cash", Amount: -2})

	// Reopening runs the schema again on a file that already has the column.
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	plaidSync(s, "2026-09-01", plaid.InvestmentTransaction{InvestmentTransactionID: "new", AccountID: "acc-ind", Date: "2026-10-01", Name: "Dividend", Type: "cash", Amount: -2})
	got := rows(s)
	want := map[string]string{"robinhood-a": "robinhood", "robinhood-b": "robinhood", "new": "plaid"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v; want %v (the Plaid row Plaid stopped sending goes, imported rows stay)", got, want)
	}
	for id, src := range want {
		if got[id] != src {
			t.Errorf("rows = %v; want %v", got, want)
		}
	}
}
