package costco

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/kilianc/fin/internal/money"
)

// Order is a costco.com order as its details page shows it. Each line's
// Cost is its amount plus its share, by amount, of everything else on the
// order (discounts, shipping, fees and tax): Costco does not say which
// line a coupon or a fee belongs to. An order's lines add up to its total.
// Error is set, and Lines and Payments are empty, when fin could not read
// the order; it is kept and read again when the parser changes.
type Order struct {
	Number, Date, Status                       string
	Merchandise, Discount, Shipping, Fees, Tax money.Cents
	Total                                      money.Cents
	Lines                                      []OrderLine
	Payments                                   []Tender
	Error                                      string
	Raw                                        json.RawMessage
}

// OrderLine is one item on an order, numbered from one.
type OrderLine struct {
	Line                    int
	Number, Title           string
	Quantity                float64
	UnitPrice, Amount, Cost money.Cents
}

// ParseOrder reads one order's details.
func ParseOrder(raw []byte) (*Order, error) {
	var r struct {
		Number      scalar   `json:"orderNumber"`
		Placed      string   `json:"orderPlacedDate"`
		Status      string   `json:"status"`
		Merchandise *dollars `json:"merchandiseTotal"`
		Discount    *dollars `json:"discountAmount"`
		Shipping    *dollars `json:"shippingAndHandling"`
		Delivery    *dollars `json:"retailDeliveryFee"`
		Grocery     *dollars `json:"grocerySurcharge"`
		Frozen      *dollars `json:"frozenSurchargeFee"`
		NonMember   *dollars `json:"nonMemberSurchargeAmount"`
		Tax         *dollars `json:"uSTaxTotal1"`
		Total       *dollars `json:"orderTotal"`
		Payments    []struct {
			Type    string   `json:"paymentType"`
			Charged *dollars `json:"totalCharged"`
		} `json:"orderPayment"`
		ShipTos []struct {
			Lines []struct {
				Number   scalar   `json:"itemNumber"`
				Title    string   `json:"itemDescription"`
				Price    *dollars `json:"price"`
				Quantity scalar   `json:"quantity"`
				Amount   *dollars `json:"merchandiseTotalAmount"`
				Line     int      `json:"lineNumber"`
			} `json:"orderLineItems"`
		} `json:"shipToAddress"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, errors.New("costco: order changed format")
	}
	if r.Number == "" || r.Total == nil {
		return nil, errors.New("costco: order is missing its number or total")
	}
	date, err := receiptDate(r.Placed, "")
	if err != nil {
		return nil, err
	}
	cents := func(d *dollars) money.Cents {
		if d == nil {
			return 0
		}
		return d.Cents
	}
	o := &Order{Number: string(r.Number), Date: date, Status: r.Status, Merchandise: cents(r.Merchandise), Discount: cents(r.Discount),
		Shipping: cents(r.Shipping), Fees: cents(r.Delivery) + cents(r.Grocery) + cents(r.Frozen) + cents(r.NonMember),
		Tax: cents(r.Tax), Total: r.Total.Cents, Raw: append(json.RawMessage(nil), raw...)}
	type line struct {
		at int
		l  OrderLine
	}
	var lines []line
	for _, st := range r.ShipTos {
		for _, it := range st.Lines {
			q, err := strconv.ParseFloat(string(it.Quantity), 64)
			if err != nil || math.IsNaN(q) || math.IsInf(q, 0) || it.Amount == nil || it.Price == nil || it.Title == "" {
				return nil, fmt.Errorf("costco: order line %d is missing its description, amount, price or quantity", len(lines)+1)
			}
			lines = append(lines, line{it.Line, OrderLine{Number: string(it.Number), Title: it.Title, Quantity: q, UnitPrice: it.Price.Cents, Amount: it.Amount.Cents}})
		}
	}
	if len(lines) == 0 {
		return nil, errors.New("costco: order has no items")
	}
	slices.SortStableFunc(lines, func(a, b line) int { return cmp.Compare(a.at, b.at) })
	weights := make([]money.Cents, len(lines))
	var sum money.Cents
	for i, l := range lines {
		l.l.Line = i + 1
		o.Lines = append(o.Lines, l.l)
		weights[i] = l.l.Amount
		sum += l.l.Amount
	}
	if sum < 0 {
		for i := range weights {
			weights[i] = -weights[i]
		}
	}
	for i, share := range money.Spread(o.Total-sum, weights) {
		o.Lines[i].Cost = o.Lines[i].Amount + share
	}
	for i, p := range r.Payments {
		if p.Charged == nil {
			return nil, errors.New("costco: order payment has no amount")
		}
		o.Payments = append(o.Payments, Tender{Tender: i + 1, Type: p.Type, Amount: p.Charged.Cents, NoBankCharge: noBankCharge(p.Type, "")})
	}
	return o, nil
}

// UnreadableOrder keeps an order fin could not read, under the number the
// order list gave it.
func UnreadableOrder(number string, raw []byte, cause error) Order {
	var r struct {
		Placed string `json:"orderPlacedDate"`
	}
	_ = json.Unmarshal(raw, &r)
	o := Order{Number: number, Error: cause.Error(), Raw: append(json.RawMessage(nil), raw...)}
	o.Date, _ = receiptDate(r.Placed, "")
	if !json.Valid(o.Raw) {
		b, _ := json.Marshal(string(raw))
		o.Raw = b
	}
	return o
}
