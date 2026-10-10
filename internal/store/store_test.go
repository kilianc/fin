package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/plaid"
)

func ptr[T any](v T) *T { return &v }

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := Path(t.TempDir(), "sandbox")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

var syncedAt = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func delta(cursor string, upserts []plaid.Transaction, removed ...string) ItemSync {
	return ItemSync{
		ItemID: "item-chase", Item: "chase", Institution: "Chase", Cursor: cursor, Status: "HISTORICAL_UPDATE_COMPLETE",
		SyncedAt: syncedAt,
		Accounts: []plaid.Account{{AccountID: "chk", Name: "Checking", Mask: ptr("0001"), Type: "depository",
			Balances: plaid.Balances{Current: ptr(23631.9805)}}},
		Upserts: upserts,
		Removed: removed,
	}
}

func tx(id, date string, amount float64) plaid.Transaction {
	return plaid.Transaction{TransactionID: id, AccountID: "chk", Date: date, Amount: amount, Name: "tx " + id,
		PersonalFinanceCategory: &plaid.PersonalFinanceCategory{Primary: "FOOD_AND_DRINK", Detailed: "FOOD_AND_DRINK_COFFEE"}}
}

func ids(txs []Transaction) []string {
	out := []string{}
	for _, t := range txs {
		out = append(out, t.TransactionID)
	}
	return out
}

func TestApplyIsIncremental(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if c, err := s.Cursor(ctx, "item-chase"); err != nil || c != "" {
		t.Fatalf("cursor before any sync = %q, %v", c, err)
	}
	if err := s.Apply(ctx, delta("c1", []plaid.Transaction{tx("a", "2026-01-02", 4.25), tx("b", "2026-01-03", 10)})); err != nil {
		t.Fatal(err)
	}
	modified := tx("a", "2026-01-02", 5.75)
	if err := s.Apply(ctx, delta("c2", []plaid.Transaction{modified, tx("c", "2026-01-04", 1)}, "b")); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Cursor(ctx, "item-chase"); c != "c2" {
		t.Errorf("cursor = %q, want c2", c)
	}
	got, err := s.Transactions(ctx, Filter{ItemIDs: []string{"item-chase"}, From: "2026-01-01", To: "2026-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []string{"c", "a"}) {
		t.Errorf("ids = %v, want [c a]", ids(got))
	}
	if a := got[1]; a.Amount != 5.75 || a.AccountName != "Checking" || a.Institution != "Chase" || *a.Category != "FOOD_AND_DRINK" {
		t.Errorf("a = %+v", a)
	}
	got, _ = s.Transactions(ctx, Filter{ItemIDs: []string{"item-chase"}, From: "2026-01-01", To: "2026-12-31", Account: "0001"})
	if len(got) != 2 {
		t.Errorf("filter by mask: %v", ids(got))
	}
	got, _ = s.Transactions(ctx, Filter{ItemIDs: []string{"item-other"}, From: "2026-01-01", To: "2026-12-31"})
	if len(got) != 0 {
		t.Errorf("other Item: %v", ids(got))
	}
}

func TestQueryIsReadOnlyAndLocal(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	if err := s.Apply(ctx, delta("c1", []plaid.Transaction{tx("a", "2026-01-02", 4.25), tx("b", "2026-02-03", 10.1)})); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("store mode = %v, %v", info.Mode(), err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	res, err := ro.Query(ctx, `select date_trunc('month', date)::date as month, sum(amount) as total, count(*) as n
		from transactions group by all order by month`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Columns, []string{"month", "total", "n"}) {
		t.Errorf("columns = %v", res.Columns)
	}
	if len(res.Rows) != 2 || res.Rows[0][0] != "2026-01-01" || res.Rows[1][1] != 10.1 || res.Rows[0][2] != int64(1) {
		t.Errorf("rows = %#v", res.Rows)
	}
	if res, err := ro.Query(ctx, `select current from accounts`); err != nil || res.Rows[0][0] != 23631.9805 {
		t.Errorf("balance lost precision: %v, %v", res, err)
	}
	for _, q := range []string{
		`delete from transactions`,
		`set enable_external_access = true`,
		`copy transactions to '` + filepath.Join(t.TempDir(), "out.csv") + `'`,
		`select * from read_csv('/etc/hosts')`,
	} {
		if _, err := ro.Query(ctx, q); err == nil {
			t.Errorf("%s: want an error", q)
		}
	}
}

func TestOpenReadOnlyWithoutStore(t *testing.T) {
	_, err := OpenReadOnly(context.Background(), Path(t.TempDir(), "production"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteItemForgetsOnlyThatItem(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	other := delta("o1", []plaid.Transaction{tx("o", "2026-01-05", 3)})
	other.ItemID, other.Item = "item-citi", "citi"
	other.Accounts[0].AccountID, other.Upserts[0].AccountID = "citi-chk", "citi-chk"
	for _, d := range []ItemSync{delta("c1", []plaid.Transaction{tx("a", "2026-01-02", 4.25)}), other} {
		if err := s.Apply(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteItem(ctx, "item-chase"); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"transactions": 1, "accounts": 1, "items": 1} {
		var n int64
		if err := s.db.QueryRowContext(ctx, `select count(*) from `+table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows, %v", table, n, err)
		}
	}
	if c, _ := s.Cursor(ctx, "item-chase"); c != "" {
		t.Errorf("cursor kept: %q", c)
	}
}
