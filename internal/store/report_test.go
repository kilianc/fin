package store

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/kilianc/fin/internal/plaid"
)

func TestBalancesKeepOneRowPerAccountPerDay(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	card := plaid.Account{AccountID: "card", Name: "Sapphire", Type: "credit", Balances: plaid.Balances{Current: ptr(120.5)}}
	for _, d := range []ItemSync{
		delta("c1", nil),
		{ItemID: "item-chase", Item: "chase", Cursor: "c2", SyncedAt: syncedAt, Accounts: []plaid.Account{card}},
		// A later sync the same day replaces that day's balance.
		{ItemID: "item-chase", Item: "chase", Cursor: "c3", SyncedAt: syncedAt.Add(3600e9),
			Accounts: []plaid.Account{{AccountID: "chk", Name: "Checking", Type: "depository", Balances: plaid.Balances{Current: ptr(100.0)}}}},
		{ItemID: "item-chase", Item: "chase", Cursor: "c4", SyncedAt: syncedAt, Day: "2026-10-10",
			Accounts: []plaid.Account{{AccountID: "chk", Name: "Checking", Type: "depository", Balances: plaid.Balances{Current: ptr(90.0)}}}},
	} {
		if err := s.Apply(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.BalanceHistory(ctx, []string{"item-chase"})
	if err != nil {
		t.Fatal(err)
	}
	want := []DayValue{
		{Date: "2026-10-09", Key: "card", Value: -120.5},
		{Date: "2026-10-09", Key: "chk", Value: 100},
		{Date: "2026-10-10", Key: "chk", Value: 90},
	}
	if !slices.Equal(got, want) {
		t.Errorf("history = %v\nwant %v (one row per account per day, the card owed)", got, want)
	}
	if err := s.DeleteItem(ctx, "item-chase"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BalanceHistory(ctx, []string{"item-chase"}); len(got) != 0 {
		t.Errorf("history after DeleteItem = %v", got)
	}
}

func TestReportsReadOnAReadOnlyStore(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if err := s.Apply(ctx, delta("c1", []plaid.Transaction{tx("a", "2026-10-02", 12)})); err != nil {
		t.Fatal(err)
	}
	s.Close()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := ro.BalanceHistory(ctx, []string{"item-chase"}); err != nil || len(got) != 1 {
		t.Errorf("BalanceHistory = %v, %v", got, err)
	}
	if got, err := ro.ItemsPaidBy(ctx, []string{"a"}); err != nil || len(got) != 0 {
		t.Errorf("ItemsPaidBy = %v, %v", got, err)
	}
	if got, err := ro.HoldingsHistory(ctx, []string{"item-chase"}, ""); err != nil || len(got) != 0 {
		t.Errorf("HoldingsHistory = %v, %v", got, err)
	}
}

// A store from before balance history starts it from the accounts table, so
// an Item that never syncs again still counts toward net worth over time.
func TestBalanceHistoryStartsFromTheAccountsOnUpgrade(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if err := s.Apply(ctx, delta("c1", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `drop table balances`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.BalanceHistory(ctx, []string{"item-chase"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []DayValue{{Date: "2026-10-09", Key: "chk", Value: 23631.9805}}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	// Later opens leave the history alone.
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BalanceHistory(ctx, []string{"item-chase"}); len(got) != 1 {
		t.Errorf("history after a second open = %v", got)
	}
}

func TestBalanceHistoryEndsWhenPlaidStopsReportingABalance(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	gone := delta("c2", nil)
	gone.Day = "2026-10-10"
	gone.Accounts[0].Balances.Current = nil
	for _, d := range []ItemSync{delta("c1", nil), gone} {
		if err := s.Apply(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.BalanceHistory(ctx, []string{"item-chase"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != 23631.9805 || got[1].Date != "2026-10-10" || !math.IsNaN(got[1].Value) {
		t.Errorf("history = %v, want the balance, then NaN from the day it went missing", got)
	}
}

// A store last written by a fin older than investments has no holdings or
// securities tables; a read-only open cannot add them.
func TestHoldingsReadEmptyOnAStoreWithoutInvestments(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if err := s.Apply(ctx, delta("c1", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `drop table holdings; drop table securities`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := ro.Holdings(ctx, []string{"item-chase"}, ""); err != nil || len(got) != 0 {
		t.Errorf("Holdings = %v, %v", got, err)
	}
	if got, err := ro.HoldingsHistory(ctx, []string{"item-chase"}, ""); err != nil || len(got) != 0 {
		t.Errorf("HoldingsHistory = %v, %v", got, err)
	}
	if got, err := ro.CashEquivalents(ctx); err != nil || len(got) != 0 {
		t.Errorf("CashEquivalents = %v, %v", got, err)
	}
}

func TestQueryLimitStopsReading(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	for _, c := range []struct {
		limit, rows int
		truncated   bool
	}{{10, 10, true}, {25, 25, false}, {0, 25, false}} {
		res, err := s.QueryLimit(ctx, `select * from range(25)`, c.limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != c.rows || res.Truncated != c.truncated {
			t.Errorf("limit %d: %d rows, truncated %v", c.limit, len(res.Rows), res.Truncated)
		}
	}
}
