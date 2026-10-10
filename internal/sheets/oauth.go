// Package sheets writes fin's data to a Google Sheet. It signs in with the
// drive.file scope, so fin can open only the spreadsheets it created, and it
// talks to the Sheets REST API directly.
package sheets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// Scope lets fin create spreadsheets and edit only those it created. It does
// not let fin see any other file in the user's Drive.
const Scope = "https://www.googleapis.com/auth/drive.file"

// OAuthClient identifies fin to Google. For a desktop app Google treats the
// client secret as public: it names the app, and the user's consent is what
// grants access.
type OAuthClient struct {
	ID       string
	Secret   string
	AuthURL  string // default Google's
	TokenURL string // default Google's
}

func (c OAuthClient) config(redirect string) *oauth2.Config {
	ep := oauth2.Endpoint{
		AuthURL:   "https://accounts.google.com/o/oauth2/auth",
		TokenURL:  "https://oauth2.googleapis.com/token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
	if c.AuthURL != "" {
		ep.AuthURL = c.AuthURL
	}
	if c.TokenURL != "" {
		ep.TokenURL = c.TokenURL
	}
	return &oauth2.Config{ClientID: c.ID, ClientSecret: c.Secret, Endpoint: ep, Scopes: []string{Scope}, RedirectURL: redirect}
}

// ErrConsentDenied means the user closed or declined Google's consent screen.
var ErrConsentDenied = errors.New("Google sign-in was cancelled")

// Login runs Google's loopback sign-in: it listens on 127.0.0.1, hands the
// consent URL to show, waits for the redirect, and returns the refresh token.
func Login(ctx context.Context, c OAuthClient, show func(url string)) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	cfg := c.config(fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port))
	state := randomHex()
	verifier := oauth2.GenerateVerifier()

	type outcome struct {
		code string
		err  error
	}
	done := make(chan outcome, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "unexpected sign-in response", http.StatusBadRequest)
			return
		}
		res := outcome{code: q.Get("code")}
		if e := q.Get("error"); e != "" || res.code == "" {
			res.err = ErrConsentDenied
		}
		page(w, res.err == nil)
		select {
		case done <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	show(cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent")))
	var res outcome
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res = <-done:
	}
	if res.err != nil {
		return "", res.err
	}
	tok, err := cfg.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", fmt.Errorf("google sign-in: %w", err)
	}
	if tok.RefreshToken == "" {
		return "", errors.New("google sign-in returned no refresh token")
	}
	return tok.RefreshToken, nil
}

func page(w http.ResponseWriter, ok bool) {
	msg := "fin can now write to its spreadsheet. You can close this tab and go back to the terminal."
	if !ok {
		msg = "Sign-in was cancelled. Go back to the terminal to try again."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>fin</title>
<body style="font:16px/1.5 -apple-system,system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#1c2733">
<h1 style="font-size:1.4rem;color:#356B8D">fin</h1><p>%s</p></body>`, html.EscapeString(msg))
}

func randomHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// IsAuthExpired reports whether Google rejected the saved refresh token, so
// the user has to sign in again.
func IsAuthExpired(err error) bool {
	var re *oauth2.RetrieveError
	return errors.As(err, &re) && re.ErrorCode == "invalid_grant"
}
