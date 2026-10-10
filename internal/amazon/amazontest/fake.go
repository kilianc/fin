// Package amazontest fakes the parts of amazon.com and Chrome that fin
// amazon uses, for tests. It does not import package amazon, so that
// package's own tests can use it.
package amazontest

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/chrome/chrometest"
)

// Server is a fake amazon.com: the payments page, the payments API in pages
// of two, and order pages.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	Rows     []json.RawMessage
	Orders   map[string][]byte // order ID -> page
	SignedIn bool
	Token    string // the session-token cookie value it accepts
	Requests map[string]int
	// Limited answers the next this many requests with 429 Too Many Requests,
	// naming RetryAfter (seconds) when it is set.
	Limited    int
	RetryAfter string
	// PagesBeforeLimit, when set, answers 429 to every payments API call
	// after this many.
	PagesBeforeLimit int
	// Classic serves Rows on the older payments page, two to a page, paged
	// by posting the page's form, as Amazon does for some accounts.
	Classic bool
}

// New starts a signed-in fake that accepts session-token "good".
func New(t *testing.T) *Server {
	f := &Server{Orders: map[string][]byte{}, SignedIn: true, Token: "good", Requests: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Payment builds one row of the payments list.
func Payment(amount, date, order, method, descriptor, status string) json.RawMessage {
	row := map[string]any{
		"formattedAmount": amount, "formattedDate": date,
		"orderData":                      []map[string]string{{"orderDisplayString": "Order #" + order, "orderDetailsUrl": "https://www.amazon.com/gp/css/summary/edit.html?orderID=" + order}},
		"statementDescriptor":            descriptor,
		"statusInfo":                     map[string]string{"status": "Approved", "label": status},
		"paymentMethodDisplayStringData": map[string]string{"paymentMethodName": method},
	}
	b, _ := json.Marshal(row)
	return b
}

// Count is how many requests a path got.
func (f *Server) Count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Requests[path]
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests[r.URL.Path]++
	if f.Limited > 0 {
		f.Limited--
		if f.RetryAfter != "" {
			w.Header().Set("Retry-After", f.RetryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if c, err := r.Cookie("session-token"); err != nil || c.Value != f.Token || !f.SignedIn {
		http.Redirect(w, r, "/ap/signin", http.StatusFound)
		return
	}
	switch r.URL.Path {
	case "/cpe/yourpayments/transactions":
		http.SetCookie(w, &http.Cookie{Name: "session-token", Value: f.Token, Path: "/", MaxAge: 3600})
		if f.Classic {
			start := 0
			if r.Method == http.MethodPost {
				r.ParseForm()
				fmt.Sscanf(r.PostForm.Get("ppw-widgetState"), "state-%d", &start)
				if _, ok := r.PostForm[fmt.Sprintf(`ppw-widgetEvent:NextPage:{"nextPageKey":"key-%d"}`, start)]; !ok {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			w.Write(f.classicPage(start))
			return
		}
		fmt.Fprint(w, `<html><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"token":"tok","manageWalletRequest":{"requestContext":{"surfaceInfo":{"a":1},"localeInfo":{"locale":"en_US"}}}}}}</script></html>`)
	case "/payments-portal/data/iris/live/v1/data/manage/get-transactions":
		if f.PagesBeforeLimit > 0 && f.Requests[r.URL.Path] > f.PagesBeforeLimit {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("X-Amzn-Upx-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Start string `json:"exclusiveStartKey"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		start := 0
		fmt.Sscanf(body.Start, "page-%d", &start)
		end, next := min(start+2, len(f.Rows)), ""
		if end < len(f.Rows) {
			next = fmt.Sprintf("page-%d", end)
		}
		rows := f.Rows[start:end]
		if rows == nil {
			rows = []json.RawMessage{}
		}
		json.NewEncoder(w).Encode(map[string]any{"displayResponse": map[string]any{"transactionsList": rows, "lastEvaluatedKey": next}})
	case "/gp/css/summary/print.html":
		if page, ok := f.Orders[r.URL.Query().Get("orderID")]; ok {
			w.Write(page)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// classicPage renders the older payments page from Rows[start:start+2].
func (f *Server) classicPage(start int) []byte {
	end := min(start+2, len(f.Rows))
	var b strings.Builder
	b.WriteString(`<html><head><title>Your Payments</title></head><body><form method="post" action="/cpe/yourpayments/transactions">`)
	fmt.Fprintf(&b, `<input type="hidden" name="ppw-widgetState" value="state-%d"><input type="hidden" name="ie" value="UTF-8">`, end)
	b.WriteString(`<div class="a-box apx-transactions-sleeve-header-container"><div class="a-box-inner"><span class="a-size-base a-text-bold">Completed</span></div></div>`)
	for _, raw := range f.Rows[start:end] {
		var row struct {
			Amount     string `json:"formattedAmount"`
			Date       string `json:"formattedDate"`
			Descriptor string `json:"statementDescriptor"`
			Orders     []struct {
				Display string `json:"orderDisplayString"`
				URL     string `json:"orderDetailsUrl"`
			} `json:"orderData"`
			Method struct {
				Name string `json:"paymentMethodName"`
			} `json:"paymentMethodDisplayStringData"`
		}
		json.Unmarshal(raw, &row)
		day, _ := time.Parse("Jan 2, 2006", row.Date)
		fmt.Fprintf(&b, `<div class="a-section apx-transaction-date-container"><span>%s</span></div>`, day.Format("January 2, 2006"))
		fmt.Fprintf(&b, `<div class="a-section a-spacing-base apx-transactions-line-item-component-container"><div class="a-row"><div class="a-column a-span9"><span class="a-size-base a-text-bold">%s</span></div><div class="a-column a-span3"><span class="a-size-base-plus a-text-bold">%s</span></div></div>`,
			html.EscapeString(row.Method.Name), row.Amount)
		for _, o := range row.Orders {
			fmt.Fprintf(&b, `<div class="a-row"><a class="a-link-normal" href="%s">%s</a></div>`, o.URL, html.EscapeString(o.Display))
		}
		fmt.Fprintf(&b, `<div class="a-row"><span class="a-size-base">%s</span></div></div>`, html.EscapeString(row.Descriptor))
	}
	b.WriteString(`<span class="a-button a-button-disabled"><input disabled="disabled" class="a-button-input" type="submit"><span>Previous Page</span></span>`)
	if end < len(f.Rows) {
		fmt.Fprintf(&b, `<span class="a-button"><input name="%s" class="a-button-input" type="submit"><span>Next Page</span></span>`,
			html.EscapeString(fmt.Sprintf(`ppw-widgetEvent:NextPage:{"nextPageKey":"key-%d"}`, end)))
	}
	b.WriteString(`</form></body></html>`)
	return []byte(b.String())
}

// ChromeDir makes synthetic profiles, with an Amazon session where named.
func ChromeDir(t *testing.T, password string, profiles map[string]string, amazon map[string]string) string {
	t.Helper()
	cookies := map[string][]*http.Cookie{}
	for profile, value := range amazon {
		cookies[profile] = []*http.Cookie{{Domain: ".amazon.com", Name: "session-token", Value: value}}
	}
	return chrometest.Dir(t, password, profiles, cookies)
}
