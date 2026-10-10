package costco_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/costco/costcotest"
	"github.com/kilianc/fin/internal/pace"
)

var testWindow = costco.Window{Start: "2026-09-01", End: "2026-09-30"}

func TestClientRefreshRotationAndSaveFailure(t *testing.T) {
	s := costcotest.New(t)
	s.Rows = append(s.Rows, costcotest.Raw(t, costcotest.Receipt("synthetic-1", "2026-09-02")))
	c := costco.NewClient(&costco.Session{RefreshToken: "synthetic-refresh"}, s.URL+"/token", s.URL+"/receipts")
	c.Gate.Wait = 0
	saved := ""
	c.Save = func(session *costco.Session) error {
		if len(s.Windows()) != 0 {
			t.Error("rotation saved after receipt request")
		}
		saved = session.RefreshToken
		return nil
	}
	rows, err := c.Receipts(context.Background(), testWindow)
	if err != nil || len(rows) != 1 || saved != "synthetic-rotated-1" {
		t.Fatalf("rows=%v saved=%s err=%v", len(rows), saved, err)
	}
	if c.Session().RefreshToken != saved || c.Session().RefreshExpires.IsZero() {
		t.Error("rotation or expiry missing")
	}
	c = costco.NewClient(&costco.Session{RefreshToken: "synthetic-refresh"}, s.URL+"/token", s.URL+"/receipts")
	c.Gate.Wait = 0
	failure := errors.New("cannot seal session")
	c.Save = func(*costco.Session) error { return failure }
	if _, err := c.Receipts(context.Background(), testWindow); !errors.Is(err, failure) || len(s.Windows()) != 1 {
		t.Fatalf("failed save continued: %v", err)
	}
}

func TestClientRejectsPartialResponsesAndExpiredSignIn(t *testing.T) {
	for _, body := range []string{`{"data":{"receiptsWithCounts":{"receipts":[]}},"errors":[{"message":"partial"}]}`, `{"data":{"receiptsWithCounts":{"receipts":null}}}`, `{}`, `<html>changed</html>`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		c := costco.NewClient(&costco.Session{IDToken: "synthetic-id", IDExpires: time.Now().Add(time.Hour)}, s.URL, s.URL)
		c.Gate.Wait = 0
		_, err := c.Receipts(context.Background(), testWindow)
		s.Close()
		if err == nil {
			t.Error("marked partial data complete")
		}
	}
	s := costcotest.New(t)
	s.RefreshStatus = 400
	c := costco.NewClient(&costco.Session{RefreshToken: "synthetic-refresh"}, s.URL+"/token", s.URL+"/receipts")
	c.Gate.Wait = 0
	if err := c.Check(context.Background()); !errors.Is(err, costco.ErrSignIn) || strings.Contains(err.Error(), "must-not-leak") {
		t.Errorf("sign-in error = %v", err)
	}
}

func TestClientRetries401OnceAndNeverRetries429BeyondPace(t *testing.T) {
	for _, retryAfter := range []string{"", "1", "7200"} {
		t.Run("retry-after-"+retryAfter, func(t *testing.T) {
			var orders, refresh int
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					refresh++
					fmt.Fprint(w, `{"id_token":"new","id_token_expires_in":3600}`)
					return
				}
				orders++
				if orders == 1 {
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(429)
			}))
			defer s.Close()
			c := costco.NewClient(&costco.Session{RefreshToken: "synthetic-refresh", IDToken: "old", IDExpires: time.Now().Add(time.Hour)}, s.URL+"/token", s.URL+"/receipts")
			c.Gate.Wait = 0
			_, err := c.Receipts(context.Background(), testWindow)
			want := 2
			if retryAfter == "1" {
				want = 3
			}
			if !errors.Is(err, pace.ErrRateLimited) || orders != want || refresh != 1 {
				t.Fatalf("orders=%d refresh=%d err=%v", orders, refresh, err)
			}
		})
	}
}

func TestClientSerializesRequestsAndHonorsCancellation(t *testing.T) {
	var active, maximum atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		if n > maximum.Load() {
			maximum.Store(n)
		}
		defer active.Add(-1)
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(w, `{"data":{"receiptsWithCounts":{"receipts":[]}}}`)
	}))
	defer s.Close()
	c := costco.NewClient(&costco.Session{IDToken: "synthetic-id", IDExpires: time.Now().Add(time.Hour)}, s.URL, s.URL)
	c.Gate.Wait = 0
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := c.Receipts(context.Background(), testWindow); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if maximum.Load() != 1 {
		t.Error("overlapping requests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Receipts(ctx, testWindow); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel = %v", err)
	}
}
