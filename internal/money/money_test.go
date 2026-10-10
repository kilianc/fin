package money

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]Cents{"$1,234.56": 123456, "-$9.99": -999, "+$5.00": 500, "$7": 700, "$0.5": 50} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := Parse("12.00 EUR"); err == nil {
		t.Error("parsed a non-dollar amount")
	}
}

func TestSpreadNeverLosesACent(t *testing.T) {
	weights := []Cents{1800, 1800, 1000, 1, 0, -300}
	for total := Cents(-500); total < 500; total += 7 {
		var sum Cents
		for _, s := range Spread(total, weights) {
			sum += s
		}
		if sum != total {
			t.Fatalf("Spread(%d) adds up to %d", total, sum)
		}
	}
	if got := Spread(999, []Cents{0, 0}); got[0] != 999 || got[1] != 0 {
		t.Errorf("weightless spread = %v", got)
	}
}

func TestUSD(t *testing.T) {
	if got := Cents(-1550).USD(); got != "−$15.50" {
		t.Errorf("USD = %q", got)
	}
	if got := FromDollars(19.999); got != 2000 {
		t.Errorf("FromDollars = %d", got)
	}
}
