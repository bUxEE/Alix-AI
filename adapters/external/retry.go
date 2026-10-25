package external

import (
	"context"
	"time"
)

// RetryPolicy bounds how a provider adapter retries a request the provider
// REFUSED before processing it (a rate limit or a 5xx answer). It never covers
// a transport failure after bytes were sent: whether the provider processed
// that request is unknown, and the router decides what follows.
type RetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt (0: none).
	MaxRetries int
	// BaseDelay doubles per attempt up to MaxDelay.
	BaseDelay time.Duration
	MaxDelay  time.Duration
	// MaxRetryAfter is the longest provider-requested wait the adapter sits
	// out. A longer one is not waited for: the error goes to the caller with
	// the retry-after attached.
	MaxRetryAfter time.Duration
}

// HardMaxRetries bounds MaxRetries whatever the configuration says.
const HardMaxRetries = 5

// Normalised returns the policy with every field inside its bounds.
func (p RetryPolicy) Normalised() RetryPolicy {
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.MaxRetries > HardMaxRetries {
		p.MaxRetries = HardMaxRetries
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = 8 * time.Second
		if p.MaxDelay < p.BaseDelay {
			p.MaxDelay = p.BaseDelay
		}
	}
	if p.MaxRetryAfter < 0 {
		p.MaxRetryAfter = 0
	}
	return p
}

// Delay says how long to wait before retry number attempt (0 for the first
// retry) and whether to retry at all. retryAfter is what the provider asked
// for (zero when it said nothing). jitter returns a value in [0, 1) and adds up
// to 25 % to the wait, so that clients do not retry in step; nil means none.
// The wait is never shorter than the provider's request, and a request longer
// than MaxRetryAfter is not retried.
func (p RetryPolicy) Delay(attempt int, retryAfter time.Duration, jitter func() float64) (time.Duration, bool) {
	p = p.Normalised()
	if attempt < 0 || attempt >= p.MaxRetries || retryAfter > p.MaxRetryAfter {
		return 0, false
	}
	d := p.BaseDelay
	for i := 0; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if jitter != nil {
		d += time.Duration(float64(d) * 0.25 * jitter())
	}
	if d < retryAfter {
		d = retryAfter
	}
	return d, true
}

// Sleep waits for d or until ctx ends, whichever comes first, and returns the
// context's error in the second case.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
