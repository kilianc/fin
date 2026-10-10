package costco

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type policyTransport func(*http.Request) (*http.Response, error)

func (f policyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefreshUsesSavedPolicy(t *testing.T) {
	c := NewClient(&Session{Policy: "B2C_1A_SSO_WCS_signup_signin_900", RefreshToken: "synthetic-refresh"}, "", OrdersURL)
	calls := 0
	c.http.Transport = policyTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != signInBase+"b2c_1a_sso_wcs_signup_signin_900/oauth2/v2.0/token" {
			t.Fatalf("wrong authority: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id_token":"synthetic-id","id_token_expires_in":900}`))}, nil
	})
	if err := c.Check(context.Background()); err != nil || calls != 1 {
		t.Fatalf("refresh: calls=%d err=%v", calls, err)
	}
	invalid := NewClient(&Session{Policy: "../other", RefreshToken: "synthetic-refresh"}, "", OrdersURL)
	invalid.http.Transport = c.http.Transport
	if err := invalid.Check(context.Background()); !errors.Is(err, ErrSignIn) || calls != 1 {
		t.Fatalf("invalid policy sent a request: calls=%d err=%v", calls, err)
	}
}
