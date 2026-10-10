// Package money holds US dollar amounts as whole cents, so amounts split
// across items add up exactly.
package money

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Cents is an amount of US cents.
type Cents int64

// Dollars is the amount as a decimal number of dollars.
func (c Cents) Dollars() float64 { return float64(c) / 100 }

// String is the amount as a plain decimal, "-12.34", for storing.
func (c Cents) String() string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// USD is the amount for people: "$12.34", or "−$12.34" for a refund.
func (c Cents) USD() string {
	if c < 0 {
		return "−$" + (-c).String()
	}
	return "$" + c.String()
}

// FromDollars rounds a decimal number of dollars, as JSON APIs send them, to
// the nearest cent.
func FromDollars(f float64) Cents { return Cents(math.Round(f * 100)) }

var pattern = regexp.MustCompile(`^([+-])?\s*\$\s*([0-9][0-9,]*)(?:\.([0-9]{1,2}))?$`)

// Parse reads "$1,234.56", "-$9.99" or "+$5.00" as signed cents.
func Parse(s string) (Cents, error) {
	m := pattern.FindStringSubmatch(strings.TrimSpace(s))
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

// Spread splits total across lines in proportion to their weights, such as
// line prices, so each line carries its share of tax, shipping and
// discounts. The cents left over from rounding go to the lines with the
// largest remainders, so the shares add up to total exactly. When the
// weights add up to nothing, the first line takes it all.
func Spread(total Cents, weights []Cents) []Cents {
	out := make([]Cents, len(weights))
	if len(weights) == 0 {
		return out
	}
	var base Cents
	for _, w := range weights {
		base += w
	}
	if base <= 0 {
		out[0] = total
		return out
	}
	type rem struct {
		i    int
		frac int64
	}
	var given Cents
	rems := make([]rem, len(weights))
	for i, w := range weights {
		num := int64(total) * int64(w)
		share, frac := num/int64(base), num%int64(base)
		if frac < 0 {
			share--
			frac += int64(base)
		}
		out[i] = Cents(share)
		given += Cents(share)
		rems[i] = rem{i, frac}
	}
	// Rounding down leaves fewer cents than there are lines.
	slices.SortStableFunc(rems, func(a, b rem) int { return cmp.Compare(b.frac, a.frac) })
	for j := 0; j < int(total-given); j++ {
		out[rems[j].i]++
	}
	return out
}
