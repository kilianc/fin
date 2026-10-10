// Package amazon reads a person's own Amazon.com payments and orders with the
// session their Chrome browser already holds. It uses Amazon's website
// endpoints, which are undocumented and can change without notice.
package amazon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Cents is an amount of US cents, so allocations add up exactly.
type Cents int64

// Dollars is the amount as a decimal number of dollars.
func (c Cents) Dollars() float64 { return float64(c) / 100 }

func (c Cents) String() string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

var moneyPattern = regexp.MustCompile(`^([+-])?\s*\$\s*([0-9][0-9,]*)(?:\.([0-9]{1,2}))?$`)

// parseMoney reads "$1,234.56", "-$9.99" or "+$5.00" as signed cents.
func parseMoney(s string) (Cents, error) {
	m := moneyPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("not a US dollar amount: %q", s)
	}
	dollars, err := strconv.ParseInt(strings.ReplaceAll(m[2], ",", ""), 10, 64)
	if err != nil {
		return 0, err
	}
	frac := m[3]
	if len(frac) == 1 {
		frac += "0"
	}
	cents, _ := strconv.ParseInt("0"+frac, 10, 64)
	v := Cents(dollars*100 + cents)
	if m[1] == "-" {
		v = -v
	}
	return v, nil
}

// Payment is one row of Amazon's "Your Payments" list: a charge to, or a
// refund onto, one payment method.
type Payment struct {
	// Key identifies the row across syncs. Amazon gives rows no ID, so it is a
	// hash of the row's fields and its position among identical rows.
	Key        string
	Date       string // YYYY-MM-DD
	Amount     Cents  // fin's sign: a charge is positive, a refund negative
	Currency   string
	Method     string // as Amazon shows it, such as "Visa" or "Amazon Gift Card"
	Descriptor string // the statement descriptor, such as "AMZN Mktp US"
	Status     string // such as "Charged" or "Refunded"
	OrderIDs   []string
	Raw        json.RawMessage
}

// GiftCard reports whether the payment never reached a bank or card account.
func (p Payment) GiftCard() bool {
	m := strings.ToLower(p.Method)
	return strings.Contains(m, "gift card") || strings.Contains(m, "points")
}

type rawPayment struct {
	FormattedAmount string `json:"formattedAmount"`
	FormattedDate   string `json:"formattedDate"`
	OrderData       []struct {
		OrderDisplayString string `json:"orderDisplayString"`
	} `json:"orderData"`
	StatementDescriptor string `json:"statementDescriptor"`
	StatusInfo          struct {
		Label string `json:"label"`
	} `json:"statusInfo"`
	PaymentMethod struct {
		Name string `json:"paymentMethodName"`
	} `json:"paymentMethodDisplayStringData"`
}

var orderIDPattern = regexp.MustCompile(`\b(?:[0-9]{3}|D[0-9]{2}|P[0-9]{2})-[0-9]{7}-[0-9]{7}\b`)

// ValidOrderID reports whether id looks like an Amazon order number.
func ValidOrderID(id string) bool {
	return orderIDPattern.MatchString(id) && len(id) == 19
}

// parsePayments turns one page of Amazon's transaction list into Payments.
// seen counts identical rows across pages, so each gets a distinct key.
func parsePayments(account string, rows []json.RawMessage, seen map[string]int) ([]Payment, error) {
	out := make([]Payment, 0, len(rows))
	for _, raw := range rows {
		var r rawPayment
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("amazon: payment row: %w", err)
		}
		amount, err := parseMoney(r.FormattedAmount)
		if err != nil {
			return nil, fmt.Errorf("amazon: payment amount: %w", err)
		}
		day, err := time.Parse("Jan 2, 2006", strings.TrimSpace(r.FormattedDate))
		if err != nil {
			return nil, fmt.Errorf("amazon: payment date %q: %w", r.FormattedDate, err)
		}
		p := Payment{
			Date: day.Format("2006-01-02"),
			// Amazon shows a charge as -$x; fin, like Plaid, counts money out as positive.
			Amount:     -amount,
			Currency:   "USD",
			Method:     strings.TrimSpace(r.PaymentMethod.Name),
			Descriptor: strings.TrimSpace(r.StatementDescriptor),
			Status:     strings.TrimSpace(r.StatusInfo.Label),
			OrderIDs:   []string{},
			Raw:        raw,
		}
		for _, o := range r.OrderData {
			if id := orderIDPattern.FindString(o.OrderDisplayString); id != "" {
				p.OrderIDs = append(p.OrderIDs, id)
			}
		}
		fields := strings.Join([]string{account, p.Date, p.Amount.String(), p.Method, p.Descriptor, p.Status, strings.Join(p.OrderIDs, ",")}, "\x00")
		n := seen[fields]
		seen[fields] = n + 1
		sum := sha256.Sum256([]byte(fields + "\x00" + strconv.Itoa(n)))
		p.Key = hex.EncodeToString(sum[:12])
		out = append(out, p)
	}
	return out, nil
}

// ErrSignIn means Amazon wants the person to sign in again.
var ErrSignIn = errors.New("Amazon no longer accepts this sign-in")
