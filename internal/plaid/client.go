// Package plaid is a small client for the read-only Plaid endpoints fin uses.
//
// It talks to the HTTP API directly instead of using the generated SDK: fin
// needs about a dozen endpoints, and a narrow surface is easy to fake in tests.
package plaid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Env is a Plaid environment.
type Env string

const (
	Sandbox    Env = "sandbox"
	Production Env = "production"
)

// ParseEnv parses PLAID_ENV. An empty value means sandbox.
func ParseEnv(s string) (Env, error) {
	switch Env(strings.ToLower(strings.TrimSpace(s))) {
	case "", Sandbox:
		return Sandbox, nil
	case Production:
		return Production, nil
	}
	return "", fmt.Errorf("PLAID_ENV must be sandbox or production, got %q", s)
}

// BaseURL is the API host for the environment.
func (e Env) BaseURL() string { return "https://" + string(e) + ".plaid.com" }

const apiVersion = "2020-09-14"

// Error is an error returned by the Plaid API.
type Error struct {
	Type           string `json:"error_type"`
	Code           string `json:"error_code"`
	Message        string `json:"error_message"`
	DisplayMessage string `json:"display_message"`
	RequestID      string `json:"request_id"`
	HTTPStatus     int    `json:"-"`
}

func (e *Error) Error() string { return fmt.Sprintf("plaid %s: %s", e.Code, e.Message) }

// IsCode reports whether err is a Plaid error with the given error_code.
func IsCode(err error, code string) bool {
	var perr *Error
	return errors.As(err, &perr) && perr.Code == code
}

// Client calls the Plaid API. Credentials travel in headers, never in URLs.
type Client struct {
	baseURL  string
	clientID string
	secret   string
	http     *http.Client
}

func NewClient(env Env, clientID, secret string) *Client {
	return &Client{
		baseURL:  env.BaseURL(),
		clientID: clientID,
		secret:   secret,
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

const maxAttempts = 3

// call POSTs in to path and decodes the response into out. Rate limits,
// 5xx responses and network errors are retried with a short backoff.
func (c *Client) call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		retry, err := c.do(ctx, path, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return lastErr
}

func (c *Client) do(ctx context.Context, path string, body []byte, out any) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Plaid-Version", apiVersion)
	req.Header.Set("PLAID-CLIENT-ID", c.clientID)
	req.Header.Set("PLAID-SECRET", c.secret)

	resp, err := c.http.Do(req)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("plaid %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return true, fmt.Errorf("plaid %s: read response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		perr := &Error{HTTPStatus: resp.StatusCode}
		if json.Unmarshal(data, perr) != nil || perr.Code == "" {
			perr.Type = "API_ERROR"
			perr.Code = "HTTP_" + strconv.Itoa(resp.StatusCode)
			perr.Message = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, perr
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("plaid %s: decode response: %w", path, err)
	}
	return false, nil
}

func (c *Client) VerifyCredentials(ctx context.Context) error {
	var out struct{}
	return c.call(ctx, "/institutions/get", map[string]any{"count": 1, "offset": 0, "country_codes": []string{"US"}}, &out)
}

func (c *Client) LinkTokenCreate(ctx context.Context, req LinkTokenCreateRequest) (*LinkTokenCreateResponse, error) {
	var out LinkTokenCreateResponse
	return &out, c.call(ctx, "/link/token/create", req, &out)
}

func (c *Client) LinkTokenGet(ctx context.Context, linkToken string) (*LinkTokenGetResponse, error) {
	var out LinkTokenGetResponse
	return &out, c.call(ctx, "/link/token/get", map[string]string{"link_token": linkToken}, &out)
}

func (c *Client) ItemPublicTokenExchange(ctx context.Context, publicToken string) (*ExchangeResponse, error) {
	var out ExchangeResponse
	return &out, c.call(ctx, "/item/public_token/exchange", map[string]string{"public_token": publicToken}, &out)
}

func (c *Client) ItemGet(ctx context.Context, accessToken string) (*ItemGetResponse, error) {
	var out ItemGetResponse
	return &out, c.call(ctx, "/item/get", map[string]string{"access_token": accessToken}, &out)
}

// ItemRemove ends an Item at Plaid and invalidates its access token.
func (c *Client) ItemRemove(ctx context.Context, accessToken string) error {
	var out struct{}
	return c.call(ctx, "/item/remove", map[string]string{"access_token": accessToken}, &out)
}

func (c *Client) InstitutionGetByID(ctx context.Context, institutionID string) (*Institution, error) {
	var out struct {
		Institution Institution `json:"institution"`
	}
	in := map[string]any{"institution_id": institutionID, "country_codes": []string{"US"}}
	return &out.Institution, c.call(ctx, "/institutions/get_by_id", in, &out)
}

// InstitutionsSearch finds US institutions by name. Products narrows the
// results to institutions that support all of them.
func (c *Client) InstitutionsSearch(ctx context.Context, query string, products []string) ([]InstitutionDetail, error) {
	in := map[string]any{
		"query":         query,
		"country_codes": []string{"US"},
		"options":       map[string]any{"include_optional_metadata": true},
	}
	if len(products) > 0 {
		in["products"] = products
	}
	var out struct {
		Institutions []InstitutionDetail `json:"institutions"`
	}
	return out.Institutions, c.call(ctx, "/institutions/search", in, &out)
}

// AccountsGet returns balances Plaid has cached, refreshed about once a day.
func (c *Client) AccountsGet(ctx context.Context, accessToken string) (*AccountsResponse, error) {
	var out AccountsResponse
	return &out, c.call(ctx, "/accounts/get", map[string]string{"access_token": accessToken}, &out)
}

// AccountsBalanceGet asks the institution for live balances. It is slower and
// billed per call in production.
func (c *Client) AccountsBalanceGet(ctx context.Context, accessToken string) (*AccountsResponse, error) {
	var out AccountsResponse
	return &out, c.call(ctx, "/accounts/balance/get", map[string]string{"access_token": accessToken}, &out)
}

func (c *Client) TransactionsSync(ctx context.Context, accessToken, cursor string, count int) (*TransactionsSyncResponse, error) {
	in := map[string]any{"access_token": accessToken, "count": count}
	if cursor != "" {
		in["cursor"] = cursor
	}
	var out TransactionsSyncResponse
	return &out, c.call(ctx, "/transactions/sync", in, &out)
}

func (c *Client) InvestmentsHoldingsGet(ctx context.Context, accessToken string) (*HoldingsResponse, error) {
	var out HoldingsResponse
	return &out, c.call(ctx, "/investments/holdings/get", map[string]string{"access_token": accessToken}, &out)
}

func (c *Client) InvestmentsTransactionsGet(ctx context.Context, accessToken, start, end string, offset, count int) (*InvestmentTransactionsResponse, error) {
	in := map[string]any{
		"access_token": accessToken,
		"start_date":   start,
		"end_date":     end,
		"options":      map[string]int{"offset": offset, "count": count},
	}
	var out InvestmentTransactionsResponse
	return &out, c.call(ctx, "/investments/transactions/get", in, &out)
}

func (c *Client) SandboxPublicTokenCreate(ctx context.Context, institutionID string, products []string) (string, error) {
	in := map[string]any{"institution_id": institutionID, "initial_products": products}
	var out struct {
		PublicToken string `json:"public_token"`
	}
	return out.PublicToken, c.call(ctx, "/sandbox/public_token/create", in, &out)
}

func (c *Client) SandboxItemResetLogin(ctx context.Context, accessToken string) error {
	var out struct{}
	return c.call(ctx, "/sandbox/item/reset_login", map[string]string{"access_token": accessToken}, &out)
}
