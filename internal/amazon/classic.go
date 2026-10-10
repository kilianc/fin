package amazon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/kilianc/fin/internal/money"
)

// Some accounts still get the older "Your Payments" page: the transactions
// are in the HTML, grouped by date, and the next page is a form post of the
// page's widget state and its Next Page button. The newer page carries a
// token for a JSON API instead.

const classicCursor = "classic:"

// classicPage reports whether page is the older payments page.
func classicPage(page []byte) bool {
	return bytes.Contains(page, []byte("apx-transactions-line-item-component-container")) ||
		bytes.Contains(page, []byte(`name="ppw-widgetState"`))
}

type classicNext struct {
	State string `json:"state"`
	Event string `json:"event"`
}

// classicPayments reads the older payments page like Payments reads the API.
func (c *Client) classicPayments(ctx context.Context, account, start string, stop func(page []Payment, next string) bool) ([]Payment, error) {
	c.mu.Lock()
	page := c.classic
	c.mu.Unlock()
	if start != "" {
		var err error
		if page, err = c.classicPost(ctx, start); err != nil {
			return nil, err
		}
	}
	out := []Payment{}
	seen := map[string]int{}
	cursors := map[string]bool{}
	for n := 0; n < 1000; n++ {
		rows, next, err := parseClassicPayments(account, page, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if stopped := stop(rows, next); stopped || next == "" {
			return out, nil
		}
		if cursors[next] {
			return nil, errors.New("amazon: the payments list repeated a page")
		}
		cursors[next] = true
		if page, err = c.classicPost(ctx, next); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("amazon: the payments list did not end after 1000 pages")
}

// classicPost presses Next Page as the browser would.
func (c *Client) classicPost(ctx context.Context, cursor string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cursor, classicCursor))
	var next classicNext
	if err != nil || !strings.HasPrefix(cursor, classicCursor) || json.Unmarshal(raw, &next) != nil {
		return nil, errors.New("amazon: the saved place in the payments list is not readable; run fin amazon sync --full")
	}
	form := url.Values{"ppw-widgetState": {next.State}, "ie": {"UTF-8"}, next.Event: {""}}
	page, err := c.send(ctx, http.MethodPost, paymentsPage, []byte(form.Encode()), "text/html", "application/x-www-form-urlencoded")
	if err != nil {
		return nil, err
	}
	if signInPage(page) {
		return nil, ErrSignIn
	}
	if !classicPage(page) {
		return nil, &PageError{Msg: "amazon: the next payments page changed; fin cannot read it", Page: page}
	}
	return page, nil
}

// parseClassicPayments reads one page of the older list, and the cursor of
// the page after it ("" on the last page).
func parseClassicPayments(account string, page []byte, seen map[string]int) ([]Payment, string, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, "", fmt.Errorf("amazon: payments page: %w", err)
	}
	out := []Payment{}
	var date, status, state, event string
	var walk func(*html.Node) error
	walk = func(n *html.Node) error {
		if n.Type == html.ElementNode {
			switch {
			case hasClass(n, "apx-transactions-sleeve-header-container"):
				status = text(n)
				return nil
			case hasClass(n, "apx-transaction-date-container"):
				day, err := time.Parse("January 2, 2006", text(n))
				if err != nil {
					return fmt.Errorf("amazon: payment date %q: %w", text(n), err)
				}
				date = day.Format("2006-01-02")
				return nil
			case hasClass(n, "apx-transactions-line-item-component-container"):
				p, err := classicRow(account, n, date, status, seen)
				if err != nil {
					return err
				}
				out = append(out, p)
				return nil
			case n.Data == "input" && attr(n, "name") == "ppw-widgetState":
				state = attr(n, "value")
			case n.Data == "input" && strings.HasPrefix(attr(n, "name"), "ppw-widgetEvent:") &&
				strings.Contains(attr(n, "name"), "nextPageKey") && attr(n, "disabled") == "":
				event = attr(n, "name")
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			if err := walk(ch); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return nil, "", err
	}
	next := ""
	if state != "" && event != "" {
		b, _ := json.Marshal(classicNext{State: state, Event: event})
		next = classicCursor + base64.RawURLEncoding.EncodeToString(b)
	}
	return out, next, nil
}

// classicRow reads one transaction: the payment method and amount, then
// its orders, then the statement descriptor.
func classicRow(account string, n *html.Node, date, status string, seen map[string]int) (Payment, error) {
	if date == "" {
		return Payment{}, errors.New("amazon: a payment on the payments page has no date")
	}
	p := Payment{Date: date, Currency: "USD", Status: status, OrderIDs: []string{}}
	var amount string
	var lines []string
	for _, span := range findAll(n, "span") {
		t := text(span)
		switch {
		case t == "":
		case hasClass(span, "a-size-base-plus"):
			amount = t
		case hasClass(span, "a-text-bold") && p.Method == "":
			p.Method = t
		case hasClass(span, "a-size-base"):
			lines = append(lines, t)
		}
	}
	for _, a := range findAll(n, "a") {
		if id := orderIDPattern.FindString(text(a)); id != "" {
			p.OrderIDs = append(p.OrderIDs, id)
		}
	}
	for _, l := range lines {
		if !orderIDPattern.MatchString(l) || len(p.OrderIDs) == 0 {
			p.Descriptor = l
		}
	}
	if p.Descriptor == "" && len(lines) > 0 {
		p.Descriptor = lines[len(lines)-1]
	}
	cents, err := money.Parse(amount)
	if err != nil {
		return Payment{}, fmt.Errorf("amazon: payment amount %q: %w", amount, err)
	}
	// Amazon shows a charge as -$x; fin, like Plaid, counts money out as positive.
	p.Amount = -cents
	p.Raw, _ = json.Marshal(map[string]any{"page": "classic", "date": date, "amount": amount, "method": p.Method,
		"descriptor": p.Descriptor, "status": status, "orders": p.OrderIDs})
	fields := strings.Join([]string{account, p.Date, p.Amount.String(), p.Method, p.Descriptor, p.Status, strings.Join(p.OrderIDs, ",")}, "\x00")
	k := seen[fields]
	seen[fields] = k + 1
	sum := sha256.Sum256([]byte(fields + "\x00" + strconv.Itoa(k)))
	p.Key = hex.EncodeToString(sum[:12])
	return p, nil
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}
