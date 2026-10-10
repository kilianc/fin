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

	"github.com/kilianc/fin/internal/money"
)

// Payment is one row of Amazon's "Your Payments" list: a charge to, or a
// refund onto, one payment method.
type Payment struct {
	// Key identifies the row across syncs. Amazon gives rows no ID, so it is a
	// hash of the row's fields and its position among identical rows.
	Key        string
	Date       string      // YYYY-MM-DD
	Amount     money.Cents // fin's sign: a charge is positive, a refund negative
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

// RetailOrderID reports whether id is an amazon.com retail order, the kind
// with an order page fin reads, rather than a digital or pay.amazon.com one.
func RetailOrderID(id string) bool {
	return len(id) > 3 && id[0] >= '0' && id[0] <= '9'
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
		amount, err := money.Parse(r.FormattedAmount)
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
