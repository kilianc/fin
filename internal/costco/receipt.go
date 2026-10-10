package costco

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
const ParserVersion = 2

// Receipt is a warehouse receipt as Costco shows it. Discounts remain lines
// of their own. Error is set, and Items and Tenders are empty, when fin
// could not read the receipt; it is kept so one odd receipt never stops a
// sync, and is read again when the parser changes.
type Receipt struct {
	Barcode, Date, DateTime                               string
	Warehouse, WarehouseName, Register, Transaction, Type string
	Subtotal, Tax, Total, Savings                         money.Cents
	Items                                                 []Item
	Tenders                                               []Tender
	Error                                                 string
	Raw                                                   json.RawMessage
}

// Item is one line, numbered from one in receipt order. Cost is its amount
// plus its share of the tax: taxable lines (tax flag Y) and their discounts
// share it in proportion to their amounts, so all costs sum to the receipt
// total, to the cent. DiscountFor is the line a discount ("/ 1234567")
// takes money off, when that item is on the receipt.
type Item struct {
	Line, DiscountFor                  int
	Number, Title, TaxFlag, Department string
	Quantity                           float64
	UnitPrice, Amount, Cost            money.Cents
}

// Tender is one payment on a receipt, numbered from one. Amount keeps
// Costco's sign. NoBankCharge marks cash, shop cards and rewards, which
// leave no bank transaction behind.
type Tender struct {
	Tender                   int
	Type, Description, Last4 string
	Amount                   money.Cents
	NoBankCharge             bool
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

var (
	lastFour = regexp.MustCompile(`^(?:[Xx*• ]*)([0-9]{4})$`)
	// A discount line's title is a slash and the item number it applies to.
	discountOf = regexp.MustCompile(`^/\s*([0-9]+)$`)
)

// noBank are the tender types paid without a bank or card account of the
// shopper's: cash, Costco cards, rewards and health-plan benefit cards.
var noBank = map[string]bool{"cash": true, "shop card": true, "costco shop card": true, "cash card": true, "costco cash card": true,
	"gift card": true, "costco gift card": true, "executive reward": true, "executive rewards": true,
	"reward certificate": true, "rewards certificate": true, "citi reward certificate": true,
	"ins benefit": true, "insurance benefit": true}

// ParseReceipt reads one warehouse receipt. Missing totals or lines that
// do not reconcile are an error, never an invented adjustment.
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
	date, err := receiptDate(r.TransactionDate, r.TransactionDateTime)
	if err != nil {
		return nil, err
	}
	o := &Receipt{Barcode: string(r.Barcode), Date: date, DateTime: r.TransactionDateTime,
		Warehouse: string(r.Warehouse), WarehouseName: r.WarehouseName, Register: string(r.Register), Transaction: string(r.Transaction), Type: r.Type,
		Subtotal: r.Subtotal.Cents, Tax: r.Tax.Cents, Total: r.Total.Cents, Raw: append(json.RawMessage(nil), raw...)}
	if r.Savings != nil {
		o.Savings = r.Savings.Cents
	}
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
	}
	if sum != o.Subtotal || o.Subtotal+o.Tax != o.Total {
		return nil, errors.New("costco: receipt items and tax do not add up to its total")
	}
	line := map[string]int{}
	for _, it := range o.Items {
		if _, ok := line[it.Number]; !ok && !discountOf.MatchString(it.Title) {
			line[it.Number] = it.Line
		}
	}
	for i, it := range o.Items {
		if m := discountOf.FindStringSubmatch(it.Title); m != nil {
			o.Items[i].DiscountFor = line[m[1]]
		}
	}
	for i, tax := range money.Spread(o.Tax, taxWeights(o.Items)) {
		o.Items[i].Cost = o.Items[i].Amount + tax
	}
	for i, t := range r.Tenders {
		if t.Amount == nil {
			return nil, errors.New("costco: tender has no amount")
		}
		tender := Tender{Tender: i + 1, Type: t.Type, Description: t.Description, Amount: t.Amount.Cents}
		if m := lastFour.FindStringSubmatch(strings.TrimSpace(t.Account)); m != nil {
			tender.Last4 = m[1]
		}
		kind := strings.TrimSpace(strings.ToLower(t.Type))
		if kind == "" {
			kind = strings.TrimSpace(strings.ToLower(t.Description))
		}
		tender.NoBankCharge = noBank[kind]
		o.Tenders = append(o.Tenders, tender)
	}
	return o, nil
}

// taxWeights spreads tax over the lines Costco taxed: tax flag Y, and the
// discounts on them. A receipt with no flagged line spreads it over all.
// Weights are signed so a return's negative tax follows its negative lines.
func taxWeights(items []Item) []money.Cents {
	taxed := func(it Item) bool {
		if it.DiscountFor > 0 {
			it = items[it.DiscountFor-1]
		}
		return strings.EqualFold(it.TaxFlag, "Y")
	}
	weights := make([]money.Cents, len(items))
	for _, all := range []bool{false, true} {
		var base money.Cents
		for i, it := range items {
			weights[i] = 0
			if all || taxed(it) {
				weights[i] = it.Amount
				base += it.Amount
			}
		}
		if base < 0 {
			for i := range weights {
				weights[i] = -weights[i]
			}
		}
		if base != 0 {
			break
		}
	}
	return weights
}

// Unreadable keeps a receipt fin could not read: its raw JSON, the reason,
// and whatever identifies it. A receipt without a barcode is keyed by its
// content, so reading it again finds the same row.
func Unreadable(raw []byte, cause error) Receipt {
	var r struct {
		Barcode             scalar `json:"transactionBarcode"`
		TransactionDate     scalar `json:"transactionDate"`
		TransactionDateTime scalar `json:"transactionDateTime"`
		Warehouse           scalar `json:"warehouseNumber"`
		WarehouseName       scalar `json:"warehouseName"`
	}
	_ = json.Unmarshal(raw, &r)
	o := Receipt{Barcode: string(r.Barcode), DateTime: string(r.TransactionDateTime), Warehouse: string(r.Warehouse),
		WarehouseName: string(r.WarehouseName), Error: cause.Error(), Raw: append(json.RawMessage(nil), raw...)}
	if o.Barcode == "" {
		sum := sha256.Sum256(raw)
		o.Barcode = "unreadable-" + hex.EncodeToString(sum[:8])
	}
	o.Date, _ = receiptDate(string(r.TransactionDate), string(r.TransactionDateTime))
	if !json.Valid(o.Raw) {
		b, _ := json.Marshal(string(raw))
		o.Raw = b
	}
	return o
}

// receiptDate is the receipt's own calendar day, as printed: the date, or
// the day part of its local date and time, never shifted by a time zone.
func receiptDate(day, dateTime string) (string, error) {
	for _, s := range []string{day, dateTime} {
		if len(s) >= 10 {
			if d, err := time.Parse("2006-01-02", s[:10]); err == nil {
				return d.Format("2006-01-02"), nil
			}
		}
		if d, err := time.Parse("1/2/2006", s); err == nil {
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
