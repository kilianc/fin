package costco_test

import (
	"errors"
	"testing"

	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/costco/costcotest"
	"github.com/kilianc/fin/internal/money"
)

func TestOrderCostsAddUpAndCouponsAreNotBankCharges(t *testing.T) {
	o, err := costco.ParseOrder(costcotest.Raw(t, costcotest.Order("1000000001", "2026-10-08")))
	if err != nil {
		t.Fatal(err)
	}
	if o.Date != "2026-10-08" || o.Total != 4581 || o.Discount != 400 || o.Tax != 384 || len(o.Lines) != 2 {
		t.Fatalf("order = %+v", o)
	}
	// Lines in Costco's line order, whatever order the response lists them.
	if o.Lines[0].Title != "SYNTHETIC CREWNECK" || o.Lines[0].Line != 1 || o.Lines[1].Quantity != 2 {
		t.Fatalf("lines = %+v", o.Lines)
	}
	var sum money.Cents
	for _, l := range o.Lines {
		sum += l.Cost
	}
	if sum != o.Total {
		t.Fatalf("costs %+v add to %d, not %d", o.Lines, sum, o.Total)
	}
	if len(o.Payments) != 2 || o.Payments[0].NoBankCharge || !o.Payments[1].NoBankCharge || o.Payments[0].Amount != 4581 {
		t.Fatalf("payments = %+v", o.Payments)
	}
}

func TestOrderUnreadableKeepsItsNumber(t *testing.T) {
	raw := costcotest.Order("1000000001", "2026-10-08")
	delete(raw, "orderTotal")
	b := costcotest.Raw(t, raw)
	_, err := costco.ParseOrder(b)
	if err == nil {
		t.Fatal("accepted an order without a total")
	}
	u := costco.UnreadableOrder("1000000001", b, err)
	if u.Number != "1000000001" || u.Date != "2026-10-08" || u.Error == "" || len(u.Lines) != 0 {
		t.Fatalf("unreadable = %+v", u)
	}
	if u := costco.UnreadableOrder("1000000002", []byte("null"), errors.New("x")); u.Number != "1000000002" || len(u.Raw) == 0 {
		t.Fatalf("null order = %+v", u)
	}
}

func TestGasReceipt(t *testing.T) {
	// Shaped like a gas station receipt: one fuel line sold by the gallon,
	// tax already in the price.
	r := map[string]any{"transactionBarcode": "synthetic-gas", "transactionDate": "2026-09-23", "receiptType": "Gas Station",
		"subTotal": 56.07, "taxes": 0.0, "total": 56.07,
		"itemArray":   []any{map[string]any{"itemNumber": "800", "itemDescription01": "REGULAR", "unit": 12.345, "itemUnitPriceAmount": 4.542, "amount": 56.07}},
		"tenderArray": []any{map[string]any{"tenderTypeName": "VISA", "amountTender": 56.07, "displayAccountNumber": "****0664"}}}
	parsed, err := costco.ParseReceipt(costcotest.Raw(t, r))
	if err != nil || parsed.Items[0].Quantity != 12.345 || parsed.Items[0].Cost != 5607 {
		t.Fatalf("gas = %+v, %v", parsed, err)
	}
}
