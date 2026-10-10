// Package pace keeps fin's requests to a retailer's website at a person's
// pace: one at a time, a few seconds apart, and none at all for a while once
// the site says it is getting too many.
package pace

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrRateLimited means the site answered "too many requests".
var ErrRateLimited = errors.New("the site is limiting requests; try again later")

// RateLimited is the error for "too many requests". It matches
// ErrRateLimited and carries the wait the site asked for, if it named one.
type RateLimited struct{ RetryAfter time.Duration }

func (e *RateLimited) Error() string { return ErrRateLimited.Error() }
func (e *RateLimited) Unwrap() error { return ErrRateLimited }

// Limited is the RateLimited error for a 429 or 503 response, nil for any
// other.
func Limited(resp *http.Response) error {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return nil
	}
	secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	return &RateLimited{RetryAfter: time.Duration(secs) * time.Second}
}

// MaxRetryAfter is the longest wait a site may ask for that Retry waits out
// before trying once more.
var MaxRetryAfter = 2 * time.Minute

// Retry runs try. When it is rate limited with a short Retry-After, Retry
// waits that out and tries once more; otherwise it returns at once, since
// asking again soon only keeps the limit in place.
func Retry(ctx context.Context, try func() error) error {
	err := try()
	var rl *RateLimited
	if errors.As(err, &rl) && rl.RetryAfter > 0 && rl.RetryAfter <= MaxRetryAfter {
		select {
		case <-time.After(rl.RetryAfter):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = try()
	}
	return err
}

// Gate lets requests through one at a time, each at least Wait after the
// last plus up to half again at random, however many goroutines share it.
// A zero Wait lets everything through, for tests.
type Gate struct {
	Wait time.Duration

	mu   sync.Mutex
	next time.Time
}

// Pass blocks until the next request may go.
func (g *Gate) Pass(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := time.Until(g.next); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.next = time.Now().Add(g.Wait)
	if g.Wait > 0 {
		g.next = g.next.Add(rand.N(g.Wait / 2))
	}
	return nil
}
