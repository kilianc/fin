package amazon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kilianc/fin/internal/amazon/amazontest"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseOrderReadsItemsAndAllocatesExactly(t *testing.T) {
	page, err := os.ReadFile("testdata/order-multi.html")
	if err != nil {
		t.Fatal(err)
	}
	o, err := ParseOrder(page)
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != "111-0000001-0000001" || o.Date != "2026-09-01" || o.CardLast4 != "1002" {
		t.Errorf("order = %s %s card %s", o.ID, o.Date, o.CardLast4)
	}
	want := struct{ sub, ship, disc, tax, gift, total, paid, refund Cents }{6000, 599, -899, 499, 1000, 5199, 6199, 1550}
	got := struct{ sub, ship, disc, tax, gift, total, paid, refund Cents }{o.Subtotal, o.Shipping, o.Discounts, o.Tax, o.GiftCard, o.Total, o.Paid, o.RefundTotal}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if len(o.Items) != 3 {
		t.Fatalf("items = %d", len(o.Items))
	}
	a, b, c := o.Items[0], o.Items[1], o.Items[2]
	if a.ASIN != "B000000001" || a.Title != "Chef Knife, 8 inch" || a.Quantity != 2 || a.UnitPrice != 2000 || a.Seller != "Example Kitchen Co" {
		t.Errorf("item 1 = %+v", a)
	}
	if b.Title != "USB-C Cable & Charger | 2-Pack" || b.Quantity != 1 || !strings.Contains(b.Return, "refund has been issued") {
		t.Errorf("item 2 = %+v", b)
	}
	// 61.99 split 40 : 15.50 : 4.50, the two rounding cents to the largest remainders.
	if a.Allocated != 4133 || b.Allocated != 1601 || c.Allocated != 465 {
		t.Errorf("allocated = %d %d %d", a.Allocated, b.Allocated, c.Allocated)
	}
	if a.Allocated+b.Allocated+c.Allocated != o.Paid {
		t.Errorf("items do not add up to %d", o.Paid)
	}
}

func TestParseOrderRejectsOtherPages(t *testing.T) {
	if _, err := ParseOrder([]byte(`<html><form name="signIn"><input id="ap_email"></form></html>`)); !errors.Is(err, ErrSignIn) {
		t.Errorf("sign-in page: err = %v", err)
	}
	if _, err := ParseOrder([]byte(`<html><body>Digital Order Summary</body></html>`)); !errors.Is(err, ErrNotOrderPage) {
		t.Errorf("other page: err = %v", err)
	}
}

func TestAllocateNeverLosesACent(t *testing.T) {
	for paid := Cents(0); paid < 500; paid += 7 {
		o := &Order{Paid: paid, Items: []Item{{UnitPrice: 333, Quantity: 1}, {UnitPrice: 333, Quantity: 1}, {UnitPrice: 334, Quantity: 3}}}
		allocate(o)
		var sum Cents
		for _, it := range o.Items {
			sum += it.Allocated
		}
		if sum != paid {
			t.Fatalf("paid %d: items add up to %d", paid, sum)
		}
	}
}

func TestParsePaymentsSignsAndKeys(t *testing.T) {
	rows := []json.RawMessage{
		amazontest.Payment("-$1,234.50", "Sep 2, 2026", "111-0000001-0000001", "Visa", "AMZN Mktp US", "Charged"),
		amazontest.Payment("+$15.50", "Sep 9, 2026", "111-0000001-0000001", "Visa", "AMZN Mktp US", "Refunded"),
		amazontest.Payment("-$9.99", "Sep 2, 2026", "D01-0000003-0000003", "Amazon Gift Card", "", "Charged"),
		amazontest.Payment("-$9.99", "Sep 2, 2026", "D01-0000003-0000003", "Amazon Gift Card", "", "Charged"),
	}
	ps, err := parsePayments("kilian", rows, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].Amount != 123450 || ps[0].Date != "2026-09-02" || ps[0].OrderIDs[0] != "111-0000001-0000001" {
		t.Errorf("charge = %+v", ps[0])
	}
	if ps[1].Amount != -1550 || ps[1].Status != "Refunded" {
		t.Errorf("refund = %+v", ps[1])
	}
	if !ps[2].GiftCard() || ps[0].GiftCard() {
		t.Error("gift card detection")
	}
	if ps[2].Key == ps[3].Key {
		t.Error("identical rows share a key")
	}
	again, _ := parsePayments("kilian", rows, map[string]int{})
	for i := range ps {
		if ps[i].Key != again[i].Key {
			t.Errorf("row %d key changed between reads", i)
		}
	}
}

func TestClientPagesStopsAndKeepsCookies(t *testing.T) {
	rows := []json.RawMessage{
		amazontest.Payment("-$24.99", "Oct 2, 2026", "112-0000002-0000002", "Visa", "AMZN Mktp US", "Charged"),
		amazontest.Payment("+$15.50", "Sep 9, 2026", "111-0000001-0000001", "Platinum Card®", "AMZN Mktp US", "Refunded"),
		amazontest.Payment("-$51.99", "Sep 2, 2026", "111-0000001-0000001", "Platinum Card®", "AMZN Mktp US", "Charged"),
		amazontest.Payment("-$9.99", "Aug 2, 2026", "D01-0000003-0000003", "Amazon Gift Card", "", "Charged"),
		amazontest.Payment("-$9.99", "Jul 2, 2026", "D01-0000004-0000004", "Amazon Gift Card", "", "Charged"),
	}
	srv := amazontest.New(t)
	srv.Rows = rows
	for _, f := range []string{"order-multi.html", "order-single.html"} {
		page, _ := os.ReadFile(filepath.Join("testdata", f))
		o, _ := ParseOrder(page)
		srv.Orders[o.ID] = page
	}
	c := NewClient(&Session{UserAgent: "test", Cookies: []*http.Cookie{{Name: "session-token", Value: "good"}}}, srv.URL)
	c.Wait = 0
	ctx := context.Background()

	all, err := c.Payments(ctx, "kilian", func([]Payment) bool { return false })
	if err != nil || len(all) != 5 {
		t.Fatalf("all: %d rows, err %v", len(all), err)
	}
	pages := 0
	some, err := c.Payments(ctx, "kilian", func([]Payment) bool { pages++; return pages == 2 })
	if err != nil || len(some) != 4 {
		t.Fatalf("stopped after two pages: %d rows, err %v", len(some), err)
	}

	page, err := c.OrderPage(ctx, "111-0000001-0000001")
	if err != nil {
		t.Fatal(err)
	}
	if o, err := ParseOrder(page); err != nil || len(o.Items) != 3 {
		t.Fatalf("order page: %v", err)
	}
	if s := c.Session("Default"); len(s.Cookies) != 1 || s.Cookies[0].Expires.IsZero() {
		t.Errorf("renewed cookie not kept: %+v", s.Cookies)
	}

	srv.SignedIn = false
	if _, err := c.Payments(ctx, "kilian", func([]Payment) bool { return false }); !errors.Is(err, ErrSignIn) {
		t.Errorf("signed out: err = %v", err)
	}
}

func TestChromeDecryptsOnlyAmazonCookies(t *testing.T) {
	dir := amazontest.ChromeDir(t, "peanuts", map[string]string{"Default": "Pat", "Profile 1": "Sam"}, map[string]string{"Default": "good"})
	asked := 0
	c := Chrome{Dir: dir, SafeStorage: func() (string, error) { asked++; return "peanuts", nil }}

	profiles, err := c.Profiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].Dir != "Default" || !profiles[0].Amazon || profiles[1].Amazon {
		t.Errorf("profiles = %+v", profiles)
	}
	if asked != 0 {
		t.Error("listing profiles asked for the Keychain password")
	}
	s, err := c.Session("Default")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cookies) != 1 || s.Cookies[0].Value != "good" || s.Cookies[0].Expires.Year() < 2026 {
		t.Errorf("cookies = %+v", s.Cookies)
	}
	if !strings.Contains(s.UserAgent, "Chrome/154.0.0.0") {
		t.Errorf("user agent = %q", s.UserAgent)
	}
	c.SafeStorage = func() (string, error) { return "wrong", nil }
	if _, err := c.Session("Default"); err == nil || !strings.Contains(err.Error(), "wrong key") {
		t.Errorf("wrong password: err = %v", err)
	}
	if _, err := c.Session("../etc"); err == nil {
		t.Error("accepted a path as a profile")
	}
}

func TestClientWaitsOutRateLimits(t *testing.T) {
	srv := amazontest.New(t)
	srv.Rows = []json.RawMessage{amazontest.Payment("-$1.00", "Oct 2, 2026", "112-0000002-0000002", "Visa", "AMZN Mktp US", "Charged")}
	c := NewClient(&Session{UserAgent: "test", Cookies: []*http.Cookie{{Name: "session-token", Value: "good"}}}, srv.URL)
	c.Wait, c.backoff = 0, []time.Duration{time.Millisecond, time.Millisecond}
	srv.Limited = 2
	if rows, err := c.Payments(context.Background(), "pat", func([]Payment) bool { return false }); err != nil || len(rows) != 1 {
		t.Fatalf("after two 429s: %d rows, %v", len(rows), err)
	}
	srv.Limited = 3
	if _, err := c.Payments(context.Background(), "pat", func([]Payment) bool { return false }); !errors.Is(err, ErrRateLimited) {
		t.Errorf("after the retries run out: err = %v", err)
	}
}
