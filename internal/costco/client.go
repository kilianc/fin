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

// graphql posts one query to Costco's orders API and returns the data of
// a 200 response. An expired ID token is renewed once; a response with
// GraphQL errors is an error, never partial data.
func (c *Client) graphql(ctx context.Context, kind, query string, variables any, data any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if c.session.IDToken == "" || time.Until(c.session.IDExpires) < 2*time.Minute {
			if err := c.refresh(ctx); err != nil {
				return err
			}
		}
		raw, status, err := c.request(ctx, c.ordersURL, kind, "application/json-patch+json", body, c.session.IDToken)
		if err != nil {
			return err
		}
		if status == 401 && attempt == 0 {
			c.session.IDToken = ""
			continue
		}
		if status == 401 || status == 403 || (status >= 300 && status < 400) {
			return ErrSignIn
		}
		if status != 200 {
			return fmt.Errorf("costco: %s: HTTP %d", kind, status)
		}
		var r struct {
			Data   json.RawMessage   `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(raw, &r) != nil || len(r.Data) == 0 || string(r.Data) == "null" {
			return fmt.Errorf("costco: %s response changed format", kind)
		}
		if len(r.Errors) > 0 {
			return fmt.Errorf("costco: %s query returned errors", kind)
		}
		if json.Unmarshal(r.Data, data) != nil {
			return fmt.Errorf("costco: %s response changed format", kind)
		}
		return nil
	}
	return ErrSignIn
}

// Receipts reads one inclusive window of warehouse, gas station and car
// wash receipts with their items. A partial response cannot mark a window
// complete.
func (c *Client) Receipts(ctx context.Context, window Window) ([]Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	start, end, err := window.format("1/02/2006")
	if err != nil {
		return nil, err
	}
	var r struct {
		Receipts *struct {
			Rows []json.RawMessage `json:"receipts"`
		} `json:"receiptsWithCounts"`
	}
	if err := c.graphql(ctx, "receipts", receiptsQuery, map[string]string{
		"startDate": start, "endDate": end, "documentType": "all", "documentSubType": "all"}, &r); err != nil {
		return nil, err
	}
	if r.Receipts == nil || r.Receipts.Rows == nil {
		return nil, errors.New("costco: receipts response changed format")
	}
	// A receipt fin cannot read is kept as unreadable rather than
	// stopping the window; a repeated barcode keeps the last copy.
	out := make([]Receipt, 0, len(r.Receipts.Rows))
	at := map[string]int{}
	for _, raw := range r.Receipts.Rows {
		receipt, err := ParseReceipt(raw)
		if err != nil {
			u := Unreadable(raw, err)
			receipt = &u
		}
		if i, ok := at[receipt.Barcode]; ok {
			out[i] = *receipt
			continue
		}
		at[receipt.Barcode] = len(out)
		out = append(out, *receipt)
	}
	return out, nil
}

// onlineWarehouse is the warehouse number costco.com orders are filed under.
const onlineWarehouse = "847"

// ordersPage is how many orders the order list returns at a time.
const ordersPage = 10

// Orders reads the costco.com orders placed in one inclusive window: the
// order list a page at a time, then each order's details.
func (c *Client) Orders(ctx context.Context, window Window) ([]Order, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	start, end, err := window.format("2006-1-02")
	if err != nil {
		return nil, err
	}
	var numbers []string
	seen := map[string]bool{}
	for page := 1; ; page++ {
		var r struct {
			Lists []struct {
				Total  int `json:"totalNumberOfRecords"`
				Orders []struct {
					Number scalar `json:"orderNumber"`
				} `json:"bcOrders"`
			} `json:"getOnlineOrders"`
		}
		if err := c.graphql(ctx, "orders", ordersQuery, map[string]any{"startDate": start, "endDate": end,
			"pageNumber": page, "pageSize": ordersPage, "warehouseNumber": onlineWarehouse}, &r); err != nil {
			return nil, err
		}
		if len(r.Lists) == 0 {
			return nil, errors.New("costco: orders response changed format")
		}
		list := r.Lists[0]
		for _, o := range list.Orders {
			if n := string(o.Number); n != "" && !seen[n] {
				seen[n] = true
				numbers = append(numbers, n)
			}
		}
		if len(list.Orders) == 0 || page*ordersPage >= list.Total {
			break
		}
	}
	out := make([]Order, 0, len(numbers))
	for _, n := range numbers {
		var r struct {
			Order json.RawMessage `json:"getOrderDetails"`
		}
		if err := c.graphql(ctx, "order", orderQuery, map[string]any{"orderNumbers": []string{n}}, &r); err != nil {
			return nil, err
		}
		order, err := ParseOrder(r.Order)
		if err != nil {
			u := UnreadableOrder(n, r.Order, err)
			order = &u
		}
		out = append(out, *order)
	}
	return out, nil
}

const ordersQuery = `query getOnlineOrders($startDate: String!, $endDate: String!, $pageNumber: Int, $pageSize: Int, $warehouseNumber: String!) {
  getOnlineOrders(startDate: $startDate, endDate: $endDate, pageNumber: $pageNumber, pageSize: $pageSize, warehouseNumber: $warehouseNumber) {
    pageNumber pageSize totalNumberOfRecords
    bcOrders { orderNumber: sourceOrderNumber orderPlacedDate: orderedDate orderTotal status }
  }
}`

const orderQuery = `query getOrderDetails($orderNumbers: [String]) {
  getOrderDetails(orderNumbers: $orderNumbers) {
    orderNumber: sourceOrderNumber orderPlacedDate: orderedDate status
    merchandiseTotal discountAmount shippingAndHandling retailDeliveryFee grocerySurcharge frozenSurchargeFee
    nonMemberSurchargeAmount uSTaxTotal1 orderTotal shopCardAppliedAmount walletShopCardAppliedAmount
    orderPayment { paymentType totalCharged }
    shipToAddress: orderShipTos {
      orderLineItems { itemNumber itemDescription: sourceItemDescription price: unitPrice quantity: orderedTotalQuantity merchandiseTotalAmount lineNumber isFeeItem }
    }
  }
}`

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
