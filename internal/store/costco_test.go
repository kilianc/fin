package store

import (
	"context"
	"testing"

	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/costco/costcotest"
	"github.com/kilianc/fin/internal/plaid"
)

func warehouseReceipt(t *testing.T, barcode, day string, amount float64) costco.Receipt {
	t.Helper()
	r := costcotest.Receipt(barcode, day)
	r["subTotal"], r["total"], r["taxes"] = amount, amount, 0.0
	item := r["itemArray"].([]any)[0].(map[string]any)
	item["amount"], item["itemUnitPriceAmount"], item["unit"] = amount, amount, 1
	r["itemArray"] = []any{item}
	r["tenderArray"].([]any)[0].(map[string]any)["amountTender"] = amount
	parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
	if err != nil {
		t.Fatal(err)
	}
	return *parsed
}

func TestCostcoMatchesOnlyBasicFacts(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	bank := func(id, day string, amount float64) plaid.Transaction {
		return plaid.Transaction{TransactionID: id, AccountID: "visa", Date: day, Amount: amount, Name: "COSTCO WHSE #0123"}
	}
	txs := []plaid.Transaction{bank("exact", "2026-09-05", 10), bank("refund", "2026-09-01", -20), bank("duplicate1", "2026-09-02", 30), bank("duplicate2", "2026-09-03", 30), bank("shared", "2026-09-02", 40), bank("wrong-card", "2026-09-02", 50), bank("too-late", "2026-09-06", 60), bank("wrong-merchant", "2026-09-02", 70), bank("authorized", "2026-09-20", 80), bank("split", "2026-09-02", 90), bank("shop-card", "2026-09-02", 100), bank("no-mask", "2026-09-02", 110)}
	txs[7].Name = "OTHER STORE"
	txs[8].AuthorizedDate = ptr("2026-09-03")
	txs[11].AccountID = "unknown"
	if err := s.Apply(ctx, ItemSync{ItemID: "bank", Item: "bank", Cursor: "c", SyncedAt: syncedAt,
		Accounts: []plaid.Account{{AccountID: "visa", Name: "Visa", Type: "credit", Mask: ptr("4242")}, {AccountID: "unknown", Name: "Unknown", Type: "credit"}}, Upserts: txs}); err != nil {
		t.Fatal(err)
	}
	r := []costco.Receipt{}
	for _, spec := range []struct {
		id     string
		amount float64
	}{{"exact", 10}, {"refund", -20}, {"duplicate", 30}, {"shared1", 40}, {"shared2", 40}, {"wrong-card", 50}, {"too-late", 60}, {"wrong-merchant", 70}, {"authorized", 80}, {"split", 90}, {"shop-card", 100}, {"no-mask", 110}, {"missing", 120}} {
		receipt := warehouseReceipt(t, spec.id, "2026-09-02", spec.amount)
		switch spec.id {
		case "wrong-card":
			receipt.CardLast4 = "9999"
		case "split":
			receipt.SplitTender = true
		case "shop-card":
			receipt.NoBankCharge = true
		}
		r = append(r, receipt)
	}
	if err := s.ApplyCostcoReceipts(ctx, "pat", "Default", r, syncedAt); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"exact": "exact", "refund": "exact", "duplicate": "ambiguous", "shared1": "ambiguous", "shared2": "ambiguous", "wrong-card": "unmatched", "too-late": "unmatched", "wrong-merchant": "unmatched", "authorized": "exact", "split": "unmatched", "shop-card": "no_bank_charge", "no-mask": "exact", "missing": "unmatched"}
	for _, row := range queryRows(t, s, `select barcode, match, transaction_id from costco_matches`) {
		id := row[0].(string)
		if row[1] != want[id] || (row[1] != "exact" && row[2] != nil) {
			t.Errorf("%s = %v; want %s", id, row, want[id])
		}
	}
	if items, err := s.RetailerItems(ctx, "costco", ItemFilter{}); err != nil || len(items) != len(r) {
		t.Fatalf("retailer items=%d err=%v", len(items), err)
	}
}

func TestCostcoCategoriesReparseAndForget(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	r, err := costco.ParseReceipt(costcotest.Raw(t, costcotest.Receipt("synthetic-1", "2026-09-02")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyCostcoReceipts(ctx, "pat", "Default", []costco.Receipt{*r}, syncedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCategories(ctx, "costco", []CategoryChange{{Item: "pat/synthetic-1#1", Category: "PERSONAL_CARE", Product: true}, {Item: "pat/synthetic-1#2", Category: "FOOD_AND_DRINK"}}, syncedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyCostcoReceipts(ctx, "pat", "Default", []costco.Receipt{*r}, syncedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `update costco_receipts set parser = 0; update costco_items set cost = 0`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReparseCostcoReceipts(ctx, "pat"); err != nil || n != 1 {
		t.Fatalf("reparse=%d %v", n, err)
	}
	items, err := s.RetailerItems(ctx, "costco", ItemFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Cost != 10.8 || items[2].Cost != -2.16 || items[1].Quantity != 1.25 || items[2].Source != "product" || *items[0].Category != "PERSONAL_CARE" {
		t.Fatalf("items=%+v", items)
	}
	if row := queryRows(t, s, `select sum(cost) = (select total from costco_receipts) from costco_items`); row[0][0] != true {
		t.Error("costs lost cents")
	}
	sums, err := s.CostcoSummaries(ctx)
	if err != nil || sums["pat"].Receipts != 1 || sums["pat"].Items != 3 || sums["pat"].Uncategorized != 0 {
		t.Fatalf("summary=%v err=%v", sums, err)
	}
	if err := s.ApplyCostcoReceipts(ctx, "sam", "Profile 1", []costco.Receipt{*r}, syncedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCostcoAccount(ctx, "pat"); err != nil {
		t.Fatal(err)
	}
	if rows := queryRows(t, s, `select (select count(*) from costco_receipts where account='pat') + (select count(*) from costco_items where account='pat') + (select count(*) from item_categories where retailer='costco') + (select count(*) from retailer_accounts where retailer='costco' and account='pat')`); rows[0][0] != int64(0) {
		t.Errorf("forget left rows: %v", rows)
	}
	if rows := queryRows(t, s, `select count(*) from retailer_items where retailer='costco' and account='sam'`); rows[0][0] != int64(3) {
		t.Error("forget removed another account")
	}
}
