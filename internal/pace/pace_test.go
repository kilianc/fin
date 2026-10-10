package pace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGateSpacesRequests(t *testing.T) {
	g := &Gate{Wait: 40 * time.Millisecond}
	start := time.Now()
	for range 3 {
		if err := g.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Errorf("three passes took %v; want at least two waits of 40ms", d)
	}
}

func TestRetryWaitsOutOnlyShortRetryAfter(t *testing.T) {
	tries := 0
	limited := func(after time.Duration, times int) func() error {
		return func() error {
			tries++
			if tries <= times {
				return &RateLimited{RetryAfter: after}
			}
			return nil
		}
	}
	if err := Retry(context.Background(), limited(time.Millisecond, 1)); err != nil || tries != 2 {
		t.Errorf("short Retry-After: %v after %d tries", err, tries)
	}
	tries = 0
	if err := Retry(context.Background(), limited(0, 1)); !errors.Is(err, ErrRateLimited) || tries != 1 {
		t.Errorf("no Retry-After: %v after %d tries", err, tries)
	}
	tries = 0
	if err := Retry(context.Background(), limited(time.Hour, 1)); !errors.Is(err, ErrRateLimited) || tries != 1 {
		t.Errorf("long Retry-After: %v after %d tries", err, tries)
	}
}
