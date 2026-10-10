package costco_test

import (
	"encoding/json"
	"errors"
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
		if len(parsed.Items) != 3 || parsed.Items[1].Quantity != 1.25 || parsed.Items[2].DiscountFor != 1 || parsed.Error != "" {
			t.Fatalf("receipt = %+v", parsed)
		}
		var sum money.Cents
		for _, it := range parsed.Items {
			sum += it.Cost
		}
		// Tax (1.04) falls on the taxed soap and its discount only: 10 and -2.
		want := []money.Cents{1130, 500, -226}
		for i, it := range parsed.Items {
			if it.Cost != money.Cents(float64(want[i])*sign) {
				t.Errorf("sign %v line %d cost = %d; want %d", sign, i+1, it.Cost, want[i])
			}
		}
		if sum != parsed.Total {
			t.Fatalf("costs = %+v; total %d", parsed.Items, parsed.Total)
		}
		if len(parsed.Tenders) != 1 || parsed.Tenders[0].Last4 != "4242" || parsed.Tenders[0].NoBankCharge || parsed.Tenders[0].Tender != 1 {
			t.Fatalf("tenders = %+v", parsed.Tenders)
		}
	}
}

func TestReceiptTaxWithoutFlagsSpreadsOverAllLines(t *testing.T) {
	r := costcotest.Receipt("synthetic-1", "2026-09-02")
	for _, item := range r["itemArray"].([]any) {
		delete(item.(map[string]any), "taxFlag")
	}
	parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
	if err != nil {
		t.Fatal(err)
	}
	var sum money.Cents
	for _, it := range parsed.Items {
		sum += it.Cost
	}
	if parsed.Items[1].Cost == parsed.Items[1].Amount || sum != parsed.Total {
		t.Fatalf("items = %+v", parsed.Items)
	}
}

func TestReceiptRejectsInventedTotals(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(r map[string]any) { delete(r, "total") },
		func(r map[string]any) { r["taxes"] = nil },
		func(r map[string]any) { r["total"] = 16.0 },
		func(r map[string]any) { r["itemArray"].([]any)[0].(map[string]any)["amount"] = 11.0 },
		func(r map[string]any) { r["transactionDate"], r["transactionDateTime"] = "unknown", "unknown" },
		func(r map[string]any) { r["itemArray"] = []any{} },
	} {
		r := costcotest.Receipt("synthetic-1", "2026-09-02")
		change(r)
		raw := costcotest.Raw(t, r)
		_, err := costco.ParseReceipt(raw)
		if err == nil {
			t.Error("accepted an incomplete or inconsistent receipt")
			continue
		}
		// Kept as unreadable, under its own barcode, with nothing invented.
		u := costco.Unreadable(raw, err)
		if u.Barcode != "synthetic-1" || u.Error == "" || len(u.Items) != 0 || len(u.Tenders) != 0 || string(u.Raw) != string(raw) {
			t.Errorf("unreadable = %+v", u)
		}
	}
	a, b := costco.Unreadable([]byte(`{"total": 1}`), errors.New("x")), costco.Unreadable([]byte(`{"total": 1}`), errors.New("y"))
	if a.Barcode == "" || a.Barcode != b.Barcode || a.Date != "" {
		t.Errorf("no barcode: %+v %+v", a, b)
	}
	if u := costco.Unreadable([]byte("not json"), errors.New("x")); !json.Valid(u.Raw) {
		t.Error("raw is not storable JSON")
	}
}

func TestReceiptDateIsTheLocalDay(t *testing.T) {
	r := costcotest.Receipt("synthetic-1", "2026-09-02")
	r["transactionDate"], r["transactionDateTime"] = nil, "2026-09-02T23:30:00-07:00"
	parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
	if err != nil || parsed.Date != "2026-09-02" {
		t.Fatalf("date = %v, %v", parsed, err)
	}
}

func TestReceiptTenders(t *testing.T) {
	for _, kind := range []string{"Costco Shop Card", "Executive Reward", "Cash"} {
		r := costcotest.Receipt("synthetic-1", "2026-09-02")
		r["tenderArray"].([]any)[0].(map[string]any)["tenderTypeName"] = kind
		parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
		if err != nil || !parsed.Tenders[0].NoBankCharge {
			t.Fatalf("%s: %+v, %v", kind, parsed, err)
		}
	}
	r := costcotest.Receipt("synthetic-1", "2026-09-02")
	r["tenderArray"].([]any)[0].(map[string]any)["amountTender"] = 10.04
	r["tenderArray"] = append(r["tenderArray"].([]any), map[string]any{"tenderTypeName": "Costco Shop Card", "amountTender": 4.0})
	parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
	if err != nil || len(parsed.Tenders) != 2 || parsed.Tenders[0].NoBankCharge || !parsed.Tenders[1].NoBankCharge ||
		parsed.Tenders[0].Amount != 1004 || parsed.Tenders[1].Tender != 2 || len(parsed.Raw) == 0 {
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
