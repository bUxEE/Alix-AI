package providers

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// MaxRetryAfter caps a provider's retry-after: a header asking for a week is
// treated as an hour, so a caller's backoff stays bounded.
const MaxRetryAfter = time.Hour

// httpDate is the RFC 7231 IMF-fixdate layout (net/http.TimeFormat), spelled
// out because this package has no HTTP.
const httpDate = "Mon, 02 Jan 2006 15:04:05 GMT"

// RateLimitInfo is what a provider said about its rate limits.
type RateLimitInfo struct {
	// RetryAfter is how long to wait before retrying (zero when not limited or
	// unknown). It is never negative and never above MaxRetryAfter.
	RetryAfter time.Duration `json:"retry_after_ns"`
	// Scope names the exhausted limit: "requests" or "toks" ("" if unknown).
	Scope string `json:"scope,omitempty"`
	// RemainingRequests and RemainingToks are nil when the provider did not
	// say.
	RemainingRequests *int `json:"remaining_requests,omitempty"`
	RemainingToks     *int `json:"remaining_toks,omitempty"`
}

// ParseRetryAfter parses a Retry-After header value: whole or decimal seconds,
// or an HTTP date relative to now. A negative, empty or unparseable value is
// not ok; a date in the past is zero. The result is clamped to MaxRetryAfter.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		switch {
		case !(secs >= 0): // NaN or negative
			return 0, false
		case secs > MaxRetryAfter.Seconds():
			return MaxRetryAfter, true
		}
		d = time.Duration(secs * float64(time.Second))
	} else if t, err := time.Parse(httpDate, v); err == nil {
		d = t.Sub(now)
		if d < 0 {
			d = 0
		}
	} else {
		return 0, false
	}
	if d > MaxRetryAfter {
		d = MaxRetryAfter
	}
	return d, true
}

// RateLimitError builds the typed rate_limited error carrying the retry-after
// (retryable, but only after the delay). The error never carries the
// provider's response body.
func RateLimitError(info RateLimitInfo) error {
	if info.RetryAfter < 0 {
		info.RetryAfter = 0
	}
	if info.RetryAfter > MaxRetryAfter {
		info.RetryAfter = MaxRetryAfter
	}
	e := contracts.New(contracts.ErrRateLimited, "providers: provider rate limit reached").
		WithDetails("retry_after_ms", info.RetryAfter.Milliseconds())
	if info.Scope != "" {
		e = e.WithDetails("limit_scope", info.Scope)
	}
	return e
}

// RateLimitFrom recovers the rate-limit info from an error built by
// RateLimitError (or by an adapter following the same shape).
func RateLimitFrom(err error) (RateLimitInfo, bool) {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrRateLimited {
		return RateLimitInfo{}, false
	}
	var info RateLimitInfo
	switch v := ce.Details["retry_after_ms"].(type) {
	case int64:
		info.RetryAfter = time.Duration(v) * time.Millisecond
	case int:
		info.RetryAfter = time.Duration(v) * time.Millisecond
	case float64:
		info.RetryAfter = time.Duration(v) * time.Millisecond
	}
	if info.RetryAfter < 0 {
		info.RetryAfter = 0
	}
	if info.RetryAfter > MaxRetryAfter {
		info.RetryAfter = MaxRetryAfter
	}
	info.Scope, _ = ce.Details["limit_scope"].(string)
	return info, true
}
