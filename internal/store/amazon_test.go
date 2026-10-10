package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/plaid"
)

func amazonTx(id, date string, amount float64) plaid.Transaction {
	return plaid.Transaction{TransactionID: id, AccountID: "amex", Date: date, Amount: amount, Name: "AMAZON MARKETPLACE NAMZN.COM/BILL",
		MerchantName:            ptr("Amazon"),
		PersonalFinanceCategory: &plaid.PersonalFinanceCategory{Primary: "GENERAL_MERCHANDISE", Detailed: "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES"}}
}

func payment(key, date string, amount amazon.Cents, method string, orders ...string) amazon.Payment {
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

func TestAmazonMatchesSplitsAndSpending(t *testing.T) {
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

	if got, _ := s.AmazonCompleteSince(ctx, "pat"); got != "2026-01-01" {
		t.Errorf("complete since = %q", got)
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

	// 51.99 split by allocated 41.33 : 16.01 : 4.65 of 61.99.
	splits := queryRows(t, s, `select line, amount from amazon_splits where transaction_id = 'charge' order by line`)
	if len(splits) != 3 || splits[0][1] != 34.66 || splits[1][1] != 13.43 || splits[2][1] != 3.9 {
		t.Errorf("charge splits = %v", splits)
	}
	// The 15.50 refund goes to the cable, the item Amazon marks refunded.
	refund := queryRows(t, s, `select line, amount from amazon_splits where transaction_id = 'refund'`)
	if len(refund) != 1 || refund[0][0] != int32(2) || refund[0][1] != -15.5 {
		t.Errorf("refund splits = %v", refund)
	}

	total := queryRows(t, s, `select sum(amount)::double, count(*) from spending`)
	if total[0][0] != 51.99-15.50+9+9+4.25 || total[0][1] != int64(3+1+3) {
		t.Errorf("spending = %v; it must keep every dollar and list items in place of matched charges", total)
	}

	items, err := s.AmazonItems(ctx, false)
	if err != nil || len(items) != 3 || items[0].Item != multi+"#1" || items[0].Source != "none" {
		t.Fatalf("uncategorized = %+v, %v", items, err)
	}
	err = s.SetAmazonCategories(ctx, []CategoryChange{
		{OrderID: multi, Line: 1, Category: "HOME_IMPROVEMENT", CategoryDetailed: "HOME_IMPROVEMENT_HARDWARE", ASINDefault: true},
		{OrderID: multi, Line: 2, Category: "GENERAL_MERCHANDISE", CategoryDetailed: "GENERAL_MERCHANDISE_ELECTRONICS"},
	}, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAmazonCategories(ctx, []CategoryChange{{OrderID: multi, Line: 9, Category: "X"}}, syncedAt); err == nil {
		t.Error("set a category on a missing item")
	}
	byCat := map[string]float64{}
	for _, r := range queryRows(t, s, `select category, sum(amount)::double from spending where transaction_id = 'charge' group by 1`) {
		byCat[r[0].(string)] = r[1].(float64)
	}
	if byCat["HOME_IMPROVEMENT"] != 34.66 || byCat["GENERAL_MERCHANDISE"] != 13.43+3.9 {
		t.Errorf("charge by category = %v", byCat)
	}
	if left, _ := s.AmazonItems(ctx, false); len(left) != 1 {
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
	if rows := queryRows(t, s, `select (select count(*) from amazon_payments) + (select count(*) from amazon_items) + (select count(*) from amazon_orders)`); rows[0][0] != int64(0) {
		t.Errorf("rows left after delete = %v", rows[0][0])
	}
}
