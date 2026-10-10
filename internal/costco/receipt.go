package costco

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/money"
)

// ParserVersion changes whenever receipt parsing changes, so kept JSON can
// be read again without asking Costco.
const ParserVersion = 1

// Receipt is a warehouse receipt as Costco shows it. Discounts remain lines
// of their own. Tenders are kept even when the whole receipt cannot match a
// single bank charge.
type Receipt struct {
	Barcode, Date, DateTime                               string
	Warehouse, WarehouseName, Register, Transaction, Type string
	Subtotal, Tax, Total, Savings                         money.Cents
	CardLast4                                             string
	NoBankCharge, SplitTender                             bool
	Items                                                 []Item
	Tenders                                               []Tender
	Raw                                                   json.RawMessage
}

// Item is one line, numbered from one in receipt order. Cost includes its
// share of tax in proportion to signed amounts; all costs sum to the
// receipt total, to the cent.
type Item struct {
	Line                               int
	Number, Title, TaxFlag, Department string
	Quantity                           float64
	UnitPrice, Amount, Cost            money.Cents
}

// Tender is a payment method on a receipt. Amount keeps Costco's sign.
type Tender struct {
	Type, Description, Last4 string
	Amount                   money.Cents
}

// dollars converts API money at the JSON boundary. All stored arithmetic
// uses cents, including values returned by the API as decimal strings.
type dollars struct{ money.Cents }

func (d *dollars) UnmarshalJSON(b []byte) error {
	var n scalar
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e7 {
		return errors.New("costco: invalid dollar amount")
	}
	d.Cents = money.FromDollars(f)
	return nil
}

// scalar accepts the strings and JSON numbers Costco uses for identifiers.
type scalar string

func (s *scalar) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var text string
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
		*s = scalar(strings.TrimSpace(text))
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*s = scalar(n.String())
	return nil
}

var lastFour = regexp.MustCompile(`^(?:[Xx*• ]*)([0-9]{4})$`)

// ParseReceipt reads one warehouse receipt. Missing totals or lines that
// do not reconcile stop the window instead of inventing an adjustment.
func ParseReceipt(raw []byte) (*Receipt, error) {
	var r struct {
		Barcode             scalar   `json:"transactionBarcode"`
		TransactionDate     string   `json:"transactionDate"`
		TransactionDateTime string   `json:"transactionDateTime"`
		Warehouse           scalar   `json:"warehouseNumber"`
		WarehouseName       string   `json:"warehouseName"`
		Register            scalar   `json:"registerNumber"`
		Transaction         scalar   `json:"transactionNumber"`
		Type                string   `json:"transactionType"`
		Subtotal            *dollars `json:"subTotal"`
		Tax                 *dollars `json:"taxes"`
		Total               *dollars `json:"total"`
		Savings             *dollars `json:"instantSavings"`
		Items               []struct {
			Number     scalar   `json:"itemNumber"`
			Desc1      string   `json:"itemDescription01"`
			Desc2      string   `json:"itemDescription02"`
			Quantity   scalar   `json:"unit"`
			UnitPrice  *dollars `json:"itemUnitPriceAmount"`
			Amount     *dollars `json:"amount"`
			TaxFlag    scalar   `json:"taxFlag"`
			Department scalar   `json:"itemDepartmentNumber"`
		} `json:"itemArray"`
		Tenders []struct {
			Type        string   `json:"tenderTypeName"`
			Description string   `json:"tenderDescription"`
			Account     string   `json:"displayAccountNumber"`
			Amount      *dollars `json:"amountTender"`
		} `json:"tenderArray"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, errors.New("costco: receipt changed format")
	}
	if r.Barcode == "" || r.Subtotal == nil || r.Tax == nil || r.Total == nil || len(r.Items) == 0 {
		return nil, errors.New("costco: receipt is missing its barcode, totals or items")
	}
	day := r.TransactionDate
	if day == "" {
		day = r.TransactionDateTime
	}
	date, err := receiptDate(day)
	if err != nil {
		return nil, err
	}
	o := &Receipt{Barcode: string(r.Barcode), Date: date, DateTime: r.TransactionDateTime,
		Warehouse: string(r.Warehouse), WarehouseName: r.WarehouseName, Register: string(r.Register), Transaction: string(r.Transaction), Type: r.Type,
		Subtotal: r.Subtotal.Cents, Tax: r.Tax.Cents, Total: r.Total.Cents, Raw: append(json.RawMessage(nil), raw...)}
	if r.Savings != nil {
		o.Savings = r.Savings.Cents
	}
	weights := make([]money.Cents, len(r.Items))
	var sum money.Cents
	for i, it := range r.Items {
		q, err := strconv.ParseFloat(string(it.Quantity), 64)
		if err != nil || math.IsNaN(q) || math.IsInf(q, 0) || it.Amount == nil || it.UnitPrice == nil {
			return nil, fmt.Errorf("costco: receipt item %d is missing its amount, price or quantity", i+1)
		}
		title := strings.TrimSpace(it.Desc1 + " " + it.Desc2)
		if title == "" {
			return nil, fmt.Errorf("costco: receipt item %d has no description", i+1)
		}
		o.Items = append(o.Items, Item{Line: i + 1, Number: string(it.Number), Title: title, Quantity: q,
			UnitPrice: it.UnitPrice.Cents, Amount: it.Amount.Cents, TaxFlag: string(it.TaxFlag), Department: string(it.Department)})
		sum += it.Amount.Cents
		weights[i] = it.Amount.Cents
		// A return has negative amounts and tax. Positive weights let Spread
		// distribute that negative tax proportionally across the return.
		if o.Subtotal < 0 {
			weights[i] = -weights[i]
		}
	}
	if sum != o.Subtotal || o.Subtotal+o.Tax != o.Total {
		return nil, errors.New("costco: receipt items and tax do not add up to its total")
	}
	for i, tax := range money.Spread(o.Tax, weights) {
		o.Items[i].Cost = o.Items[i].Amount + tax
	}
	var tenderTotal money.Cents
	nonzero, nonbank := 0, 0
	for _, t := range r.Tenders {
		if t.Amount == nil {
			return nil, errors.New("costco: tender has no amount")
		}
		tender := Tender{Type: t.Type, Description: t.Description, Amount: t.Amount.Cents}
		if m := lastFour.FindStringSubmatch(strings.TrimSpace(t.Account)); m != nil {
			tender.Last4 = m[1]
		}
		o.Tenders = append(o.Tenders, tender)
		if tender.Amount == 0 {
			continue
		}
		nonzero++
		tenderTotal += tender.Amount
		kind := strings.TrimSpace(strings.ToLower(t.Type))
		if kind == "" {
			kind = strings.TrimSpace(strings.ToLower(t.Description))
		}
		switch kind {
		case "cash", "shop card", "costco shop card", "cash card", "costco cash card", "gift card", "costco gift card",
			"executive reward", "executive rewards", "reward certificate", "rewards certificate", "citi reward certificate":
			nonbank++
		}

		o.CardLast4 = tender.Last4
	}
	o.NoBankCharge = o.Total == 0 || (nonzero > 0 && nonzero == nonbank && tenderTotal == o.Total)
	// Split or inconsistent tenders cannot identify a bank transaction for
	// the whole receipt. Do not attach any one card's last four to it.
	o.SplitTender = nonzero > 1 || (len(r.Tenders) > 0 && tenderTotal != o.Total)
	if o.SplitTender || o.NoBankCharge {
		o.CardLast4 = ""
	}
	return o, nil
}

func receiptDate(s string) (string, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02", "1/2/2006"} {
		if d, err := time.Parse(layout, s); err == nil {
			return d.Format("2006-01-02"), nil
		}
	}
	return "", errors.New("costco: receipt date changed format")
}

// Window is an inclusive date range. Windows walks backwards at most 90
// days at a time, without gaps or overlapping endpoints.
type Window struct{ Start, End string }

func Windows(since, until string) ([]Window, error) {
	start, err := time.Parse("2006-01-02", since)
	if err != nil {
		return nil, err
	}
	end, err := time.Parse("2006-01-02", until)
	if err != nil {
		return nil, err
	}
	out := []Window{}
	for !end.Before(start) {
		from := end.AddDate(0, 0, -89)
		if from.Before(start) {
			from = start
		}
		out = append(out, Window{from.Format("2006-01-02"), end.Format("2006-01-02")})
		end = from.AddDate(0, 0, -1)
	}
	return out, nil
}
