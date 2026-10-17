package store

import (
	"context"
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
