package costco

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kilianc/fin/internal/pace"
)

// Client reads receipts and renews one session. Calls are serialized,
// including refreshes, and every request passes through Gate and Retry.
type Client struct {
	mu                  sync.Mutex
	session             Session
	http                *http.Client
	tokenURL, ordersURL string
	Gate                *pace.Gate
	Log                 func(kind string, status int)
	// Save persists rotation before any subsequent receipt request. A failed
	// save stops the read, so a token is never silently lost on interruption.
	Save func(*Session) error
}

func NewClient(s *Session, tokenURL, ordersURL string) *Client {
	if tokenURL == "" {
		tokenURL = TokenURL
		if s.Policy != "" {
			tokenURL = ""
			if signInPolicy.MatchString(s.Policy) {
				tokenURL = signInBase + strings.ToLower(s.Policy) + "/oauth2/v2.0/token"
			}
		}
	}
	return &Client{session: *s, tokenURL: tokenURL, ordersURL: ordersURL,
		Gate: &pace.Gate{Wait: 3 * time.Second},
		http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Check verifies a newly imported sign-in with one refresh grant.
func (c *Client) Check(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refresh(ctx)
}

func (c *Client) Session() *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.session
	return &s
}

// request never reports response bodies: token errors can contain session
// material, and receipt errors can contain personal data.
func (c *Client) request(ctx context.Context, endpoint, kind, contentType string, body []byte, bearer string) ([]byte, int, error) {
	var data []byte
	var status int
	err := pace.Retry(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.Gate.Pass(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("User-Agent", c.session.UserAgent)
		req.Header.Set("Origin", Origin)
		req.Header.Set("Referer", Origin+"/")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		if bearer != "" {
			req.Header.Set("costco-x-authorization", "Bearer "+bearer)
			req.Header.Set("costco-x-wcs-clientid", "4900eb1f-0c10-4bd9-99c3-c59e6c1ecebf")
			req.Header.Set("client-identifier", "481b1aec-aa3b-454b-b81b-48187e28f205")
			req.Header.Set("costco.env", "ecom")
			req.Header.Set("costco.service", "restOrders")
		}
		resp, err := c.http.Do(req)
		status = 0
		if resp != nil {
			status = resp.StatusCode
		}
		if c.Log != nil {
			c.Log(kind, status)
		}
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := pace.Limited(resp); err != nil {
			return err
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 32<<20 {
			return errors.New("costco: response larger than 32 MiB")
		}
		return nil
	})
	return data, status, err
}

func (c *Client) refresh(ctx context.Context) error {
	if c.session.RefreshToken == "" || c.tokenURL == "" {
		return ErrSignIn
	}
	form := url.Values{"client_id": {spaID}, "scope": {"openid profile offline_access"}, "grant_type": {"refresh_token"}, "refresh_token": {c.session.RefreshToken}}
	data, status, err := c.request(ctx, c.tokenURL, "refresh", "application/x-www-form-urlencoded", []byte(form.Encode()), "")
	if err != nil {
		return err
	}
	if status == 401 || status == 403 || (status >= 300 && status < 400) {
		return ErrSignIn
	}
	var r struct {
		IDToken        string `json:"id_token"`
		RefreshToken   string `json:"refresh_token"`
		IDExpires      scalar `json:"id_token_expires_in"`
		RefreshExpires scalar `json:"refresh_token_expires_in"`
		Error          string `json:"error"`
	}
	if json.Unmarshal(data, &r) != nil {
		return errors.New("costco: token response changed format")
	}
	if r.Error == "invalid_grant" || r.Error == "interaction_required" {
		return ErrSignIn
	}
	if status != 200 {
		return fmt.Errorf("costco: refresh: HTTP %d", status)
	}
	if r.IDToken == "" {
		return errors.New("costco: token response has no id_token")
	}
	now := time.Now().UTC()
	s := c.session
	s.IDToken, s.SavedAt = r.IDToken, now
	seconds := func(s scalar) time.Duration { n, _ := time.ParseDuration(string(s) + "s"); return n }
	s.IDExpires = now.Add(seconds(r.IDExpires))
	// This is only reading expiry, not verifying or trusting JWT claims.
	if parts := strings.Split(r.IDToken, "."); len(parts) == 3 {
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Exp int64 `json:"exp"`
		}
		if json.Unmarshal(b, &claims) == nil && claims.Exp > 0 {
			s.IDExpires = time.Unix(claims.Exp, 0).UTC()
		}
	}
	if r.RefreshToken != "" {
		s.RefreshToken = r.RefreshToken
	}
	if expiry := seconds(r.RefreshExpires); expiry > 0 {
		s.RefreshExpires = now.Add(expiry)
	}
	c.session = s
	if c.Save != nil {
		return c.Save(&s)
	}
	return nil
}

// Receipts reads one inclusive window of warehouse receipts with their
// items. A partial GraphQL response cannot mark a window complete.
func (c *Client) Receipts(ctx context.Context, window Window) ([]Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	format := func(day string) (string, error) {
		d, err := time.Parse("2006-01-02", day)
		return d.Format("1/02/2006"), err
	}
	start, err := format(window.Start)
	if err != nil {
		return nil, err
	}
	end, err := format(window.End)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"query": receiptsQuery, "variables": map[string]string{
		"startDate": start, "endDate": end, "documentType": "warehouse", "documentSubType": "all"}})
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if c.session.IDToken == "" || time.Until(c.session.IDExpires) < 2*time.Minute {
			if err := c.refresh(ctx); err != nil {
				return nil, err
			}
		}
		data, status, err := c.request(ctx, c.ordersURL, "receipts", "application/json-patch+json", body, c.session.IDToken)
		if err != nil {
			return nil, err
		}
		if status == 401 && attempt == 0 {
			c.session.IDToken = ""
			continue
		}
		if status == 401 || status == 403 || (status >= 300 && status < 400) {
			return nil, ErrSignIn
		}
		if status != 200 {
			return nil, fmt.Errorf("costco: receipts: HTTP %d", status)
		}
		var r struct {
			Data struct {
				Receipts *struct {
					Rows []json.RawMessage `json:"receipts"`
				} `json:"receiptsWithCounts"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(data, &r) != nil || r.Data.Receipts == nil || r.Data.Receipts.Rows == nil {
			return nil, errors.New("costco: receipts response changed format")
		}
		if len(r.Errors) > 0 {
			return nil, errors.New("costco: receipts query returned errors")
		}
		out := make([]Receipt, 0, len(r.Data.Receipts.Rows))
		seen := map[string]bool{}
		for _, raw := range r.Data.Receipts.Rows {
			receipt, err := ParseReceipt(raw)
			if err != nil {
				return nil, err
			}
			if receipt.Date < window.Start || receipt.Date > window.End || seen[receipt.Barcode] {
				return nil, errors.New("costco: receipts response repeats a receipt or falls outside the requested window")
			}
			seen[receipt.Barcode] = true
			out = append(out, *receipt)
		}
		return out, nil
	}
	return nil, ErrSignIn
}

const receiptsQuery = `query receiptsWithCounts($startDate: String!, $endDate: String!, $documentType: String!, $documentSubType: String!) {
  receiptsWithCounts(startDate: $startDate, endDate: $endDate, documentType: $documentType, documentSubType: $documentSubType) {
    receipts {
      warehouseName receiptType documentType transactionDateTime transactionDate
      warehouseNumber registerNumber transactionNumber transactionType transactionBarcode
      total subTotal taxes instantSavings
      itemArray { itemNumber itemDescription01 itemDescription02 itemIdentifier itemDepartmentNumber unit amount taxFlag itemUnitPriceAmount }
      tenderArray { tenderTypeCode tenderSubTypeCode tenderDescription amountTender displayAccountNumber sequenceNumber tenderTypeName tenderEntryMethodDescription }
    }
  }
}`
