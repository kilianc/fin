package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/money"
	"github.com/kilianc/fin/internal/plaid"
)

func amazonTx(id, date string, amount float64) plaid.Transaction {
	return plaid.Transaction{TransactionID: id, AccountID: "amex", Date: date, Amount: amount, Name: "AMAZON MARKETPLACE NAMZN.COM/BILL",
		MerchantName:            ptr("Amazon"),
		PersonalFinanceCategory: &plaid.PersonalFinanceCategory{Primary: "GENERAL_MERCHANDISE", Detailed: "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES"}}
}

func payment(key, date string, amount money.Cents, method string, orders ...string) amazon.Payment {
	return amazon.Payment{Key: key, Date: date, Amount: amount, Currency: "USD", Method: method, Descriptor: "AMZN Mktp US",
		Status: "Charged", OrderIDs: orders, Raw: json.RawMessage(`{}`)}
}

func queryRows(t *testing.T, s *Store, q string) [][]any {
	t.Helper()
	res, err := s.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res.Rows
}

func TestAmazonMatchesAndCategories(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	d := ItemSync{ItemID: "item-amex", Item: "amex", Cursor: "c", SyncedAt: syncedAt,
		Accounts: []plaid.Account{{AccountID: "amex", Name: "Platinum", Mask: ptr("1002"), Type: "credit"}},
		Upserts: []plaid.Transaction{
			amazonTx("charge", "2026-09-03", 51.99),
			amazonTx("refund", "2026-09-10", -15.50),
			amazonTx("twin-1", "2026-08-01", 9.00),
			amazonTx("twin-2", "2026-08-02", 9.00),
			tx("coffee", "2026-09-05", 4.25),
		}}
	d.Upserts[4].AccountID = "amex"
	if err := s.Apply(ctx, d); err != nil {
		t.Fatal(err)
	}
	multi := "111-0000001-0000001"
	payments := []amazon.Payment{
		payment("p-charge", "2026-09-02", 5199, "Platinum Card®", multi),
		payment("p-refund", "2026-09-09", -1550, "Platinum Card®", multi),
		payment("p-gift", "2026-09-02", 1000, "Amazon Gift Card", multi),
		payment("p-twin", "2026-08-01", 900, "Visa", "112-0000009-0000009"),
		payment("p-lost", "2026-07-01", 3000, "Visa", "113-0000010-0000010"),
		payment("p-digital", "2026-07-01", 999, "Visa", "D01-0000011-0000011"),
	}
	if err := s.ApplyAmazonPayments(ctx, "pat", "Default", payments, syncedAt); err != nil {
		t.Fatal(err)
	}

	if err := s.PruneAmazonPayments(ctx, "pat", "2026-01-01", map[string]bool{"p-x": true}); err != nil {
		t.Fatal(err)
	}
	if rows := queryRows(t, s, `select count(*) from amazon_payments`); rows[0][0] != int64(0) {
		t.Fatalf("prune kept %v rows", rows[0][0])
	}
	if err := s.ApplyAmazonPayments(ctx, "pat", "Default", payments, syncedAt); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, p := range payments {
		keep[p.Key] = true
	}
	if err := s.PruneAmazonPayments(ctx, "pat", "2026-01-01", keep); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.RetailerAccountState(ctx, "amazon", "pat"); got.CompleteSince != "2026-01-01" || got.Status != "ok" {
		t.Errorf("account state = %+v", got)
	}
	if got, _ := s.EarliestTransaction(ctx); got != "2026-08-01" {
		t.Errorf("earliest transaction = %q", got)
	}

	since := syncedAt.AddDate(0, 0, -60)
	fetch, err := s.AmazonOrdersToFetch(ctx, "pat", nil, since, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(fetch) != 3 || fetch[len(fetch)-1] != multi {
		t.Errorf("to fetch = %v, want the three physical orders", fetch)
	}

	page, err := os.ReadFile("../amazon/testdata/order-multi.html")
	if err != nil {
		t.Fatal(err)
	}
	o, err := amazon.ParseOrder(page)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAmazonOrder(ctx, "pat", multi, page, o, nil, syncedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAmazonOrder(ctx, "pat", "112-0000009-0000009", []byte("<html>?</html>"), nil, amazon.ErrNotOrderPage, syncedAt); err != nil {
		t.Fatal(err)
	}
	fetch, _ = s.AmazonOrdersToFetch(ctx, "pat", nil, since, syncedAt)
	if len(fetch) != 1 || fetch[0] != "113-0000010-0000010" {
		t.Errorf("after reading two orders, to fetch = %v", fetch)
	}
	fetch, _ = s.AmazonOrdersToFetch(ctx, "pat", []string{multi}, since, syncedAt.Add(-time.Hour))
	if len(fetch) != 2 {
		t.Errorf("a new payment for an order should read it again: %v", fetch)
	}

	got := map[string][2]any{}
	for _, r := range queryRows(t, s, `select payment_key, match, transaction_id from amazon_matches`) {
		got[r[0].(string)] = [2]any{r[1], r[2]}
	}
	want := map[string][2]any{
		"p-charge":  {"exact", "charge"},
		"p-refund":  {"exact", "refund"},
		"p-gift":    {"no_bank_charge", nil},
		"p-twin":    {"ambiguous", nil},
		"p-lost":    {"unmatched", nil},
		"p-digital": {"unmatched", nil},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: match = %v, want %v", k, got[k], w)
		}
	}

	uncategorized := ItemFilter{Uncategorized: true}
	items, err := s.RetailerItems(ctx, "amazon", uncategorized)
	if err != nil || len(items) != 3 || items[0].Item != multi+"#1" || items[0].Source != "none" ||
		items[0].Transaction == nil || *items[0].Transaction != "AMAZON MARKETPLACE NAMZN.COM/BILL · 2026-09-03" {
		t.Fatalf("uncategorized = %+v, %v", items, err)
	}
	err = s.SetCategories(ctx, "amazon", []CategoryChange{
		{Item: multi + "#1", Category: "HOME_IMPROVEMENT", CategoryDetailed: "HOME_IMPROVEMENT_HARDWARE", Product: true},
		{Item: multi + "#2", Category: "GENERAL_MERCHANDISE", CategoryDetailed: "GENERAL_MERCHANDISE_ELECTRONICS"},
	}, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCategories(ctx, "amazon", []CategoryChange{{Item: multi + "#9", Category: "X"}}, syncedAt); err == nil {
		t.Error("set a category on a missing item")
	}
	cats := map[string]any{}
	for _, r := range queryRows(t, s, `select item, category from retailer_items`) {
		cats[r[0].(string)] = r[1]
	}
	if cats[multi+"#1"] != "HOME_IMPROVEMENT" || cats[multi+"#2"] != "GENERAL_MERCHANDISE" || cats[multi+"#3"] != nil {
		t.Errorf("categories = %v", cats)
	}
	if left, _ := s.RetailerItems(ctx, "amazon", uncategorized); len(left) != 1 {
		t.Errorf("uncategorized after setting two = %d", len(left))
	}

	sums, err := s.AmazonSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a := sums["pat"]; a.Payments != 6 || a.Orders != 2 || a.Unreadable != 1 || a.Items != 3 || a.Uncategorized != 1 || a.Matched != 2 || a.Ambiguous != 1 {
		t.Errorf("summary = %+v", a)
	}

	// A newer parser reads kept pages again without asking Amazon.
	if _, err := s.db.ExecContext(ctx, `update amazon_orders set parser = 0`); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReparseAmazonOrders(ctx, amazon.ParseOrder)
	if err != nil || n != 2 {
		t.Fatalf("reparsed %d, %v", n, err)
	}
	if rows := queryRows(t, s, `select count(*) from amazon_items`); rows[0][0] != int64(3) {
		t.Errorf("items after reparse = %v", rows[0][0])
	}

	if err := s.DeleteAmazonAccount(ctx, "pat"); err != nil {
		t.Fatal(err)
	}
	if rows := queryRows(t, s, `select (select count(*) from amazon_payments) + (select count(*) from amazon_items) + (select count(*) from amazon_orders) +
		(select count(*) from item_categories) + (select count(*) from retailer_accounts)`); rows[0][0] != int64(0) {
		t.Errorf("rows left after delete = %v", rows[0][0])
	}
}
