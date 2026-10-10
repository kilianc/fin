package costco_test

import (
	"testing"

	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/costco/costcotest"
	"github.com/kilianc/fin/internal/money"
)

func TestReceiptAmountsDiscountsAndReturns(t *testing.T) {
	for _, sign := range []float64{1, -1} {
		r := costcotest.Receipt("synthetic-1", "2026-09-02")
		for _, field := range []string{"total", "subTotal", "taxes", "instantSavings"} {
			r[field] = r[field].(float64) * sign
		}
		for _, item := range r["itemArray"].([]any) {
			it := item.(map[string]any)
			it["amount"], it["itemUnitPriceAmount"] = it["amount"].(float64)*sign, it["itemUnitPriceAmount"].(float64)*sign
		}
		r["tenderArray"].([]any)[0].(map[string]any)["amountTender"] = 14.04 * sign
		parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
		if err != nil {
			t.Fatal(err)
		}
		if len(parsed.Items) != 3 || parsed.CardLast4 != "4242" || parsed.Items[1].Quantity != 1.25 {
			t.Fatalf("receipt = %+v", parsed)
		}
		var sum money.Cents
		for _, it := range parsed.Items {
			sum += it.Cost
		}
		if sum != parsed.Total || parsed.Items[2].Cost != money.Cents(-216*sign) {
			t.Fatalf("costs = %+v; total %d", parsed.Items, parsed.Total)
		}
		if parsed.NoBankCharge || parsed.SplitTender {
			t.Fatal("ordinary card receipt not matchable")
		}
	}
}

func TestReceiptRejectsInventedTotals(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(r map[string]any) { delete(r, "total") },
		func(r map[string]any) { r["taxes"] = nil },
		func(r map[string]any) { r["total"] = 16.0 },
		func(r map[string]any) { r["itemArray"].([]any)[0].(map[string]any)["amount"] = 11.0 },
		func(r map[string]any) { r["transactionDate"] = "unknown" },
		func(r map[string]any) { r["itemArray"] = []any{} },
	} {
		r := costcotest.Receipt("synthetic-1", "2026-09-02")
		change(r)
		if _, err := costco.ParseReceipt(costcotest.Raw(t, r)); err == nil {
			t.Error("accepted an incomplete or inconsistent receipt")
		}
	}
}

func TestReceiptTendersAndRawJSON(t *testing.T) {
	for _, kind := range []string{"Costco Shop Card", "Executive Reward", "Cash"} {
		r := costcotest.Receipt("synthetic-1", "2026-09-02")
		r["tenderArray"].([]any)[0].(map[string]any)["tenderTypeName"] = kind
		parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
		if err != nil || !parsed.NoBankCharge || parsed.CardLast4 != "" {
			t.Fatalf("%s: %+v, %v", kind, parsed, err)
		}
	}
	r := costcotest.Receipt("synthetic-1", "2026-09-02")
	tender := r["tenderArray"].([]any)[0].(map[string]any)
	tender["amountTender"] = 10.04
	r["tenderArray"] = append(r["tenderArray"].([]any), map[string]any{"tenderTypeName": "Costco Shop Card", "amountTender": 4.0})
	parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
	if err != nil || !parsed.SplitTender || parsed.NoBankCharge || parsed.CardLast4 != "" || len(parsed.Raw) == 0 {
		t.Fatalf("split: %+v, %v", parsed, err)
	}
}

func TestWindowsCoverEpochNewestFirst(t *testing.T) {
	windows, err := costco.Windows("2025-12-25", "2026-10-10")
	if err != nil {
		t.Fatal(err)
	}
	want := []costco.Window{{"2026-07-13", "2026-10-10"}, {"2026-04-14", "2026-07-12"}, {"2026-01-14", "2026-04-13"}, {"2025-12-25", "2026-01-13"}}
	if len(windows) != len(want) {
		t.Fatalf("windows = %v", windows)
	}
	for i, w := range windows {
		if w != want[i] {
			t.Errorf("window %d = %v; want %v", i, w, want[i])
		}
	}
	if one, _ := costco.Windows("2024-02-29", "2024-02-29"); len(one) != 1 || one[0].Start != one[0].End {
		t.Errorf("leap day: %v", one)
	}
	if _, err := costco.Windows("invalid", "2026-10-10"); err == nil {
		t.Error("invalid date accepted")
	}
}
