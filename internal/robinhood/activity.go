// Package robinhood reads the account activity reports Robinhood lets you
// download, one account per CSV, to fill in history older than Plaid keeps.
package robinhood

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

var header = []string{"Activity Date", "Process Date", "Settle Date", "Instrument", "Description", "Trans Code", "Quantity", "Price", "Amount"}

// Activity is one row of a report, with Plaid's signs: a positive Amount is
// money out of the account, and a sale has a negative Quantity.
type Activity struct {
	Key         string
	Date        string // YYYY-MM-DD, the activity date
	Ticker      string // "" for cash activity
	Security    string // the instrument's name, from the description
	CUSIP       string
	Description string
	Code        string // Robinhood's transaction code, such as CDIV
	Type        string // Plaid's investment transaction type
	Subtype     string
	Quantity    float64
	Price       float64
	Amount      float64
}

// kinds maps Robinhood's codes to Plaid's types and subtypes. Codes not
// listed are cash, miscellaneous.
var kinds = map[string][2]string{
	"Buy":  {"buy", "buy"},
	"Sell": {"sell", "sell"},
	"CDIV": {"cash", "dividend"},
	"INT":  {"cash", "interest"},
	"ROC":  {"cash", "return of capital"},
	"CFIR": {"cash", "contribution"},
	"MTCH": {"cash", "contribution"},
	"SLIP": {"cash", "miscellaneous"},
	"SPL":  {"transfer", "stock split"},
	"RRRD": {"transfer", "transfer"},
	"RFRC": {"transfer", "transfer"},
	"CONV": {"transfer", "transfer"}, // Apex Clearing to Robinhood Securities, Nov 2018
	"CRRD": {"transfer", "transfer"}, // conversion to a Roth IRA
	"CFRI": {"transfer", "transfer"}, // conversion from a traditional IRA
	"MINT": {"fee", "margin expense"},
	"DFEE": {"fee", "fee"},
	"GOLD": {"fee", "account fee"},
	"DTAX": {"fee", "tax withheld"},
}

// ParseActivity reads one report. account goes into each row's Key, so the
// same row in two reports of one account gets the same key.
func ParseActivity(account string, r io.Reader) ([]Activity, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("robinhood: read report: %w", err)
	}
	// A byte order mark before the first quoted header trips encoding/csv.
	cr := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte("\ufeff"))))
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("robinhood: read report: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("robinhood: the report is empty")
	}
	if !slices.Equal(rows[0], header) {
		return nil, fmt.Errorf("robinhood: not an account activity report: the columns are %q", rows[0])
	}
	out := []Activity{}
	seen := map[string]int{}
	for i, r := range rows[1:] {
		// The report ends with a blank row and a disclaimer.
		if len(r) < len(header) || strings.TrimSpace(r[0]) == "" {
			continue
		}
		a, err := parseRow(r)
		if err != nil {
			return nil, fmt.Errorf("robinhood: row %d: %w", i+2, err)
		}
		fields := strings.Join(append([]string{account}, r[:len(header)]...), "\x00")
		n := seen[fields]
		seen[fields] = n + 1
		sum := sha256.Sum256([]byte(fields + "\x00" + strconv.Itoa(n)))
		a.Key = "robinhood-" + hex.EncodeToString(sum[:12])
		out = append(out, a)
	}
	return out, nil
}

func parseRow(r []string) (Activity, error) {
	day, err := time.Parse("1/2/2006", strings.TrimSpace(r[0]))
	if err != nil {
		return Activity{}, fmt.Errorf("activity date %q", r[0])
	}
	a := Activity{Date: day.Format("2006-01-02"), Ticker: strings.TrimSpace(r[3]), Code: strings.TrimSpace(r[5])}
	lines := strings.Split(strings.TrimSpace(r[4]), "\n")
	a.Description = strings.TrimSpace(lines[0])
	for _, l := range lines[1:] {
		if c, ok := strings.CutPrefix(strings.TrimSpace(l), "CUSIP:"); ok {
			a.CUSIP = strings.TrimSpace(c)
		}
	}
	if a.Ticker != "" {
		a.Security = a.Description
	}
	kind, ok := kinds[a.Code]
	if !ok {
		kind = [2]string{"cash", "miscellaneous"}
	}
	a.Type, a.Subtype = kind[0], kind[1]
	if a.Quantity, err = number(r[6]); err != nil {
		return Activity{}, fmt.Errorf("quantity %q", r[6])
	}
	if a.Price, err = number(r[7]); err != nil {
		return Activity{}, fmt.Errorf("price %q", r[7])
	}
	amount, err := number(r[8])
	if err != nil {
		return Activity{}, fmt.Errorf("amount %q", r[8])
	}
	// Robinhood shows money out as ($x); Plaid counts it as positive.
	a.Amount = -amount
	if a.Code == "Sell" {
		a.Quantity = -a.Quantity
	}
	if a.Code == "ACH" {
		a.Subtype = "deposit"
		if a.Amount > 0 {
			a.Subtype = "withdrawal"
		}
	}
	return a, nil
}

// number reads "$1,234.56", "($1,234.56)" or "12.5"; "" is zero.
func number(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	neg := strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")")
	s = strings.Trim(s, "()")
	s = strings.NewReplacer("$", "", ",", "").Replace(s)
	v, err := strconv.ParseFloat(s, 64)
	if neg {
		v = -v
	}
	return v, err
}
