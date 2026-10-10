package amazon

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ParserVersion changes whenever ParseOrder reads pages differently, so a
// sync can re-parse the pages it kept.
const ParserVersion = 1

// Order is one Amazon order as its order page shows it.
type Order struct {
	ID        string
	Date      string // YYYY-MM-DD
	Subtotal  Cents  // Item(s) Subtotal
	Shipping  Cents  // Shipping & Handling
	Discounts Cents  // coupons, promotions and free shipping, as a negative amount
	Tax       Cents
	// Paid is what the order cost before gift cards and points: the amount
	// its items are allocated.
	Paid Cents
	// GiftCard is the part paid with a gift card balance or points, positive.
	GiftCard    Cents
	Total       Cents // Grand Total, what the cards were charged
	RefundTotal Cents
	CardLast4   string
	// Summary keeps every line of the order summary, by label.
	Summary map[string]Cents
	Items   []Item
}

// Item is one line of an order.
type Item struct {
	Line      int // 1-based, in page order
	ASIN      string
	Title     string
	Quantity  int
	UnitPrice Cents
	Seller    string
	Condition string
	Return    string // the return or refund status Amazon shows, if any
	// Allocated is the item's share of Paid: its price plus its part of
	// shipping, discounts and tax. An order's items add up to Paid exactly.
	Allocated Cents
}

// ErrNotOrderPage means the page is not a physical order's details page:
// a digital order, a sign-in page, or a layout fin cannot read.
var ErrNotOrderPage = errors.New("not a readable Amazon order page")

var (
	asinPattern  = regexp.MustCompile(`/dp/([A-Z0-9]{10})`)
	last4Pattern = regexp.MustCompile(`(?:•|\*){2,}\s*([0-9]{4})`)
	labelPattern = regexp.MustCompile(`^(.*?):?\s*([+-]?\s*\$[0-9][0-9,]*(?:\.[0-9]{1,2})?)$`)
)

// ParseOrder reads an order page saved from /gp/css/summary/print.html. It
// relies on the data-component names Amazon gives each part of the page.
func ParseOrder(page []byte) (*Order, error) {
	if signInPage(page) {
		return nil, ErrSignIn
	}
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("amazon: order page: %w", err)
	}
	comps := map[string][]*html.Node{}
	type named struct {
		name string
		node *html.Node
	}
	var ordered []named
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if c := attr(n, "data-component"); c != "" {
				comps[c] = append(comps[c], n)
				ordered = append(ordered, named{c, n})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	first := func(name string) string {
		if ns := comps[name]; len(ns) > 0 {
			return text(ns[0])
		}
		return ""
	}
	o := &Order{ID: strings.TrimSpace(first("orderId")), Summary: map[string]Cents{}, Items: []Item{}}
	if !ValidOrderID(o.ID) || len(comps["itemTitle"]) == 0 || len(comps["chargeSummary"]) == 0 {
		return nil, ErrNotOrderPage
	}
	day, err := time.Parse("January 2, 2006", first("orderDate"))
	if err != nil {
		return nil, fmt.Errorf("amazon: order %s date %q: %w", o.ID, first("orderDate"), err)
	}
	o.Date = day.Format("2006-01-02")
	if m := last4Pattern.FindStringSubmatch(first("viewPaymentPlanSummaryWidget")); m != nil {
		o.CardLast4 = m[1]
	}

	for _, li := range findAll(comps["chargeSummary"][0], "li") {
		m := labelPattern.FindStringSubmatch(text(li))
		if m == nil {
			continue
		}
		v, err := parseMoney(m[2])
		if err != nil {
			continue
		}
		o.Summary[strings.TrimSpace(m[1])] = v
	}
	for label, v := range o.Summary {
		l := strings.ToLower(label)
		switch {
		case strings.HasPrefix(l, "item(s) subtotal"):
			o.Subtotal = v
		case strings.HasPrefix(l, "shipping & handling"):
			o.Shipping = v
		case strings.HasPrefix(l, "estimated tax"), l == "tax":
			o.Tax = v
		case strings.HasPrefix(l, "grand total"):
			o.Total = v
		case strings.HasPrefix(l, "total before tax"):
		case strings.Contains(l, "gift card") || strings.Contains(l, "points"):
			o.GiftCard += -v
		case v < 0:
			o.Discounts += v
		}
	}
	if m := regexp.MustCompile(`Refund Total\s*([+-]?\$[0-9][0-9,]*(?:\.[0-9]{1,2})?)`).FindStringSubmatch(text(doc)); m != nil {
		o.RefundTotal, _ = parseMoney(m[1])
	}

	// purchasedItems wraps a shipment, which can hold several items. Each
	// item starts at its own left grid (image and quantity), followed by its
	// right grid (title, seller, price), so split the page at the left grids.
	start := "purchasedItemsLeftGrid"
	if len(comps[start]) != len(comps["itemTitle"]) {
		start = "itemTitle"
	}
	var bounds []int
	for i, c := range ordered {
		if c.name == start {
			bounds = append(bounds, i)
		}
	}
	for k, from := range bounds {
		to := len(ordered)
		if k+1 < len(bounds) {
			to = bounds[k+1]
		}
		it := Item{Line: k + 1, Quantity: 1}
		inside := func(name string) *html.Node {
			for _, c := range ordered[from:to] {
				if c.name == name {
					return c.node
				}
			}
			return nil
		}
		if t := inside("itemTitle"); t != nil {
			it.Title = text(t)
			for _, a := range findAll(t, "a") {
				if m := asinPattern.FindStringSubmatch(attr(a, "href")); m != nil {
					it.ASIN = m[1]
					break
				}
			}
		}
		if p := inside("unitPrice"); p != nil {
			for _, s := range findAll(p, "span") {
				if strings.Contains(attr(s, "class"), "a-offscreen") {
					it.UnitPrice, _ = parseMoney(text(s))
					break
				}
			}
			if it.UnitPrice == 0 {
				it.UnitPrice, _ = parseMoney(strings.Fields(text(p) + " ")[0])
			}
		}
		// The quantity is a badge on the item's image, shown only above one.
		if img := inside("itemImage"); img != nil {
			for _, d := range findAll(img, "div") {
				if strings.Contains(attr(d, "class"), "od-item-view-qty") {
					if q, err := strconv.Atoi(text(d)); err == nil && q > 0 {
						it.Quantity = q
					}
				}
			}
		}
		if s := inside("orderedMerchant"); s != nil {
			it.Seller = strings.TrimSpace(strings.TrimPrefix(text(s), "Sold by:"))
		}
		if c := inside("itemCondition"); c != nil {
			it.Condition = text(c)
		}
		if r := inside("itemReturnEligibility"); r != nil {
			it.Return = text(r)
		}
		if it.Title == "" {
			return nil, fmt.Errorf("amazon: order %s item %d has no title: %w", o.ID, it.Line, ErrNotOrderPage)
		}
		o.Items = append(o.Items, it)
	}
	o.Paid = o.Total + o.GiftCard
	allocate(o)
	return o, nil
}

// allocate splits Paid across the items in proportion to their prices, with
// the cents left over from rounding going to the largest lines first, so the
// items add up to Paid exactly.
func allocate(o *Order) {
	var base Cents
	for _, it := range o.Items {
		base += it.UnitPrice * Cents(it.Quantity)
	}
	if len(o.Items) == 0 {
		return
	}
	if base <= 0 {
		// Free items: give everything to the first so the total still adds up.
		o.Items[0].Allocated = o.Paid
		return
	}
	type rem struct {
		i    int
		frac int64
	}
	var given Cents
	rems := make([]rem, len(o.Items))
	for i, it := range o.Items {
		line := int64(it.UnitPrice) * int64(it.Quantity)
		num := int64(o.Paid) * line
		share := num / int64(base)
		frac := num % int64(base)
		if frac < 0 {
			share--
			frac += int64(base)
		}
		o.Items[i].Allocated = Cents(share)
		given += Cents(share)
		rems[i] = rem{i, frac}
	}
	// Rounding down leaves fewer cents than there are items.
	slices.SortStableFunc(rems, func(a, b rem) int { return cmp.Compare(b.frac, a.frac) })
	for j := 0; j < int(o.Paid-given); j++ {
		o.Items[rems[j].i].Allocated++
	}
}

func signInPage(page []byte) bool {
	l := bytes.ToLower(page)
	return bytes.Contains(l, []byte(`id="ap_email"`)) || bytes.Contains(l, []byte(`id="captchacharacters"`)) ||
		bytes.Contains(l, []byte("/errors/validatecaptcha")) || bytes.Contains(l, []byte(`name="signin"`))
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// text is the node's visible text with whitespace collapsed.
func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
			b.WriteByte(' ')
		case html.ElementNode:
			if n.Data == "script" || n.Data == "style" {
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func findAll(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func contains(parent, n *html.Node) bool {
	for ; n != nil; n = n.Parent {
		if n == parent {
			return true
		}
	}
	return false
}
