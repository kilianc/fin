package amazon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kilianc/fin/internal/pace"
)

// Origin is Amazon.com's website.
const Origin = "https://www.amazon.com"

const (
	paymentsPage = "/cpe/yourpayments/transactions"
	paymentsAPI  = "/payments-portal/data/iris/live/v1/data/manage/get-transactions"
	orderPage    = "/gp/css/summary/print.html"
	maxBody      = 16 << 20
)

// Session is what fin keeps of an Amazon sign-in: the amazon.com cookies
// and the browser's user agent, so requests look like the browser they came
// from.
type Session struct {
	UserAgent string         `json:"user_agent"`
	Cookies   []*http.Cookie `json:"cookies"`
	Profile   string         `json:"profile"` // the Chrome profile directory
	SavedAt   time.Time      `json:"saved_at"`
}

// Client talks to Amazon.com with one Session. It is safe for concurrent use.
type Client struct {
	base string
	http *http.Client
	ua   string

	mu      sync.Mutex
	cookies map[string]*http.Cookie
	token   string
	expires time.Time
	request pageProps
	trace   string
	// Gate spaces requests to Amazon; tests set its Wait to zero.
	Gate pace.Gate
	// Log, if set, hears about every request: which kind ("page",
	// "payments" or "order") and the HTTP status, 0 when none came back.
	Log func(kind string, status int)
}

// NewClient returns a Client for base (Origin outside tests).
func NewClient(s *Session, base string) *Client {
	c := &Client{
		base: strings.TrimRight(base, "/"), ua: s.UserAgent, cookies: map[string]*http.Cookie{},
		http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		trace: newTraceID(),
		Gate:  pace.Gate{Wait: 3 * time.Second},
	}
	for _, ck := range s.Cookies {
		c.cookies[ck.Name] = ck
	}
	return c
}

// Session returns the session with any cookies Amazon renewed.
func (c *Client) Session(profile string) *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &Session{UserAgent: c.ua, Profile: profile, SavedAt: time.Now().UTC()}
	for _, ck := range c.cookies {
		s.Cookies = append(s.Cookies, ck)
	}
	return s
}

// ErrRateLimited means Amazon answered "too many requests"; the error is a
// *pace.RateLimited.
var ErrRateLimited = pace.ErrRateLimited

func (c *Client) do(ctx context.Context, method, path string, body []byte, accept string) ([]byte, error) {
	var b []byte
	err := pace.Retry(ctx, func() error {
		var err error
		b, err = c.once(ctx, method, path, body, accept)
		return err
	})
	return b, err
}

func (c *Client) log(path string, status int) {
	if c.Log == nil {
		return
	}
	kind := "order"
	switch strings.SplitN(path, "?", 2)[0] {
	case paymentsPage:
		kind = "page"
	case paymentsAPI:
		kind = "payments"
	}
	c.Log(kind, status)
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, accept string) ([]byte, error) {
	if err := c.Gate.Pass(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", accept)
	c.mu.Lock()
	now := time.Now()
	for _, ck := range c.cookies {
		if ck.Expires.IsZero() || ck.Expires.After(now) {
			req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
		}
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", Origin)
		req.Header.Set("Referer", Origin+paymentsPage)
		req.Header.Set("X-Amzn-Upx-Token", c.token)
	}
	c.mu.Unlock()

	resp, err := c.http.Do(req)
	if err != nil {
		c.log(path, 0)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("amazon: request failed: %w", err)
	}
	defer resp.Body.Close()
	c.log(path, resp.StatusCode)
	c.keepCookies(resp.Cookies())
	if err := pace.Limited(resp); err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403 || (resp.StatusCode >= 300 && resp.StatusCode < 400):
		return nil, ErrSignIn
	case resp.StatusCode == 404:
		return nil, fmt.Errorf("amazon: %s: not found", path)
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("amazon: %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("amazon: read response: %w", err)
	}
	if len(b) > maxBody {
		return nil, errors.New("amazon: response larger than 16 MiB")
	}
	return b, nil
}

// keepCookies records cookies Amazon set or cleared, so the renewed session
// can be saved.
func (c *Client) keepCookies(set []*http.Cookie) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ck := range set {
		if ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(time.Now())) {
			delete(c.cookies, ck.Name)
			continue
		}
		if ck.MaxAge > 0 {
			ck.Expires = time.Now().Add(time.Duration(ck.MaxAge) * time.Second)
		}
		c.cookies[ck.Name] = &http.Cookie{Name: ck.Name, Value: ck.Value, Path: "/", Domain: ".amazon.com", Expires: ck.Expires, Secure: true}
	}
}

type pageProps struct {
	Token   string `json:"token"`
	Request struct {
		Context struct {
			SurfaceInfo map[string]any `json:"surfaceInfo"`
			LocaleInfo  struct {
				Locale string `json:"locale"`
			} `json:"localeInfo"`
		} `json:"requestContext"`
	} `json:"manageWalletRequest"`
}

var nextData = regexp.MustCompile(`(?is)<script\b[^>]*\bid\s*=\s*["']__NEXT_DATA__["'][^>]*>(.*?)</script\s*>`)

// Check loads the payments page, which also proves the session is signed in.
func (c *Client) Check(ctx context.Context) error { return c.bootstrap(ctx) }

// bootstrap reads the short-lived API token from the payments page.
func (c *Client) bootstrap(ctx context.Context) error {
	page, err := c.do(ctx, http.MethodGet, paymentsPage, nil, "text/html")
	if err != nil {
		return err
	}
	m := nextData.FindSubmatch(page)
	if m == nil {
		if signInPage(page) {
			return ErrSignIn
		}
		return errors.New("amazon: the payments page changed; fin cannot read it")
	}
	var data struct {
		Props struct {
			PageProps pageProps `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(m[1], &data); err != nil {
		return errors.New("amazon: the payments page data changed format")
	}
	p := data.Props.PageProps
	if p.Token == "" {
		return ErrSignIn
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.request = p.Token, p
	c.expires = time.Now().Add(20 * time.Minute)
	if parts := strings.Split(p.Token, "."); len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 {
				c.expires = time.Unix(claims.Exp, 0)
			}
		}
	}
	return nil
}

// Payments reads the payments list, newest first, page by page, from the
// page after start (the top when empty). Each page is handed to stop with
// the key of the page after it ("" at the end); stop says whether to read on.
func (c *Client) Payments(ctx context.Context, account, start string, stop func(page []Payment, next string) bool) ([]Payment, error) {
	if err := c.bootstrap(ctx); err != nil {
		return nil, err
	}
	out := []Payment{}
	seen := map[string]int{}
	cursors := map[string]bool{}
	cursor := start
	for page := 0; page < 1000; page++ {
		c.mu.Lock()
		stale := time.Until(c.expires) < time.Minute
		c.mu.Unlock()
		if stale {
			if err := c.bootstrap(ctx); err != nil {
				return nil, err
			}
		}
		body, err := c.paymentsPage(ctx, cursor)
		if errors.Is(err, ErrSignIn) {
			// The token may have expired early; fetch a fresh one once.
			if err = c.bootstrap(ctx); err == nil {
				body, err = c.paymentsPage(ctx, cursor)
			}
		}
		if err != nil {
			return nil, err
		}
		var resp struct {
			Display *struct {
				List []json.RawMessage `json:"transactionsList"`
				Next string            `json:"lastEvaluatedKey"`
			} `json:"displayResponse"`
		}
		if err := json.Unmarshal(body, &resp); err != nil || resp.Display == nil || resp.Display.List == nil {
			return nil, errors.New("amazon: the payments list changed format")
		}
		rows, err := parsePayments(account, resp.Display.List, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		next := resp.Display.Next
		// stop sees every page, the last one included.
		if stopped := stop(rows, next); stopped || next == "" {
			return out, nil
		}
		if cursors[next] {
			return nil, errors.New("amazon: the payments list repeated a page")
		}
		cursors[next] = true
		cursor = next
	}
	return nil, errors.New("amazon: the payments list did not end after 1000 pages")
}

func (c *Client) paymentsPage(ctx context.Context, cursor string) ([]byte, error) {
	c.mu.Lock()
	locale := c.request.Request.Context.LocaleInfo.Locale
	if locale == "" {
		locale = "en_US"
	}
	payload := map[string]any{
		"type": "GetTransactions", "locale": locale,
		"surfaceInfo":                c.request.Request.Context.SurfaceInfo,
		"requestTraceId":             newTraceID(),
		"applicationInstanceTraceId": c.trace,
		"transactionsViewRequest":    map[string]any{"filtersControls": map[string]any{"includeFilters": true}},
		"widgetName":                 "ViewTransactions",
	}
	c.mu.Unlock()
	if cursor != "" {
		payload["exclusiveStartKey"] = cursor
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, paymentsAPI, body, "application/json")
}

// OrderPage downloads an order's printable details page.
func (c *Client) OrderPage(ctx context.Context, id string) ([]byte, error) {
	if !ValidOrderID(id) {
		return nil, fmt.Errorf("amazon: %q is not an order number", id)
	}
	page, err := c.do(ctx, http.MethodGet, orderPage+"?orderID="+url.QueryEscape(id), nil, "text/html")
	if err != nil {
		return nil, err
	}
	if signInPage(page) {
		return nil, ErrSignIn
	}
	return page, nil
}

func newTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
