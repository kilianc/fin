package robinhood

import (
	"strings"
	"testing"
)

// report is a synthetic account activity report in Robinhood's layout.
const report = "\ufeff" + `"Activity Date","Process Date","Settle Date","Instrument","Description","Trans Code","Quantity","Price","Amount"
"3/4/2025","3/4/2025","3/5/2025","VTI","Vanguard Total Stock Market ETF
CUSIP: 922908769","Buy","2.5","$250.00","($625.00)"
"3/10/2025","3/10/2025","3/11/2025","VTI","Vanguard Total Stock Market ETF
CUSIP: 922908769","Sell","1","$260.00","$260.00"
"3/20/2025","3/20/2025","3/20/2025","VTI","Cash Div: R/D 2025-03-18 P/D 2025-03-20 - 1.5 shares at 0.9","CDIV","","","$1.35"
"3/31/2025","3/31/2025","3/31/2025","","Gold Subscription Fee","GOLD","","","($5.00)"
"4/1/2025","4/1/2025","4/1/2025","","ACH Deposit","ACH","","","$1,000.00"
"4/1/2025","4/1/2025","4/1/2025","","ACH Deposit","ACH","","","$1,000.00"
"4/2/2025","4/2/2025","4/2/2025","","Something new","ZZZ","","","$1.00"
""
"","","","","","","","","","The data provided is for informational purposes only."
`

func TestParseActivityUsesPlaidsSigns(t *testing.T) {
	rows, err := ParseActivity("acct", strings.NewReader(report))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 7 {
		t.Fatalf("got %d rows, want 7", len(rows))
	}
	buy, sell, div, fee, dep := rows[0], rows[1], rows[2], rows[3], rows[4]
	if buy.Date != "2025-03-04" || buy.Type != "buy" || buy.Quantity != 2.5 || buy.Price != 250 || buy.Amount != 625 ||
		buy.Ticker != "VTI" || buy.CUSIP != "922908769" || buy.Security != "Vanguard Total Stock Market ETF" {
		t.Errorf("buy = %+v", buy)
	}
	if sell.Quantity != -1 || sell.Amount != -260 || sell.Type != "sell" {
		t.Errorf("sell = %+v", sell)
	}
	if div.Type != "cash" || div.Subtype != "dividend" || div.Amount != -1.35 || div.Ticker != "VTI" {
		t.Errorf("dividend = %+v", div)
	}
	if fee.Type != "fee" || fee.Amount != 5 {
		t.Errorf("fee = %+v", fee)
	}
	if dep.Subtype != "deposit" || dep.Amount != -1000 || dep.Key == rows[5].Key {
		t.Errorf("identical deposits = %+v, %+v", dep, rows[5])
	}
	if rows[6].Type != "cash" || rows[6].Subtype != "miscellaneous" || rows[6].Code != "ZZZ" {
		t.Errorf("unknown code = %+v", rows[6])
	}
	again, _ := ParseActivity("acct", strings.NewReader(report))
	other, _ := ParseActivity("other", strings.NewReader(report))
	if again[0].Key != buy.Key || other[0].Key == buy.Key {
		t.Error("keys must repeat for one account and differ between accounts")
	}
}

func TestParseActivityRejectsOtherFiles(t *testing.T) {
	if _, err := ParseActivity("acct", strings.NewReader("Date,Amount\n1/1/2025,1\n")); err == nil {
		t.Error("accepted a file that is not an activity report")
	}
}
