package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// maxErrorBody bounds how much of a failure body is read for classification.
const maxErrorBody = external.ExtMaxErrorBody

// Every error this adapter returns carries two details besides its code:
//
//	provider_error_kind  the schema's ProviderErrorKind (authentication_failed,
//	                     rate_limited, quota_exceeded, ...), so the router's
//	                     sender can classify the failure without guessing;
//	sent                 whether bytes of the request may have reached the
//	                     provider: false only for a refused dial, a refused
//	                     route and local validation.
//
// A response body is used to choose the classification and is NEVER copied into
// a message or a detail: it can echo prompt text, and an error body that
// repeats a header must not reach a log. The messages are fixed phrases.

func apiErr(code contracts.ErrCode, kind pv.ProviderErrorKind, sent bool, msg string) *contracts.Error {
	return external.ExtAPIError(ProviderName, code, kind, sent, msg)
}

func malformed(msg string) *contracts.Error { return external.ExtMalformed(ProviderName, msg) }

var (
	safeStatusRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	safeReasonRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	quotaIDRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,160}$`)
)

// apiErrorBody is the error envelope (google.rpc.Status):
//
//	{"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED","details":[
//	  {"@type":".../google.rpc.ErrorInfo","reason":"API_KEY_INVALID"},
//	  {"@type":".../google.rpc.QuotaFailure","violations":[{"quotaId":"...PerDay..."}]},
//	  {"@type":".../google.rpc.RetryInfo","retryDelay":"34s"}]}}
//
// Only status, the ErrorInfo reason, the quota ids and the retry delay are read
// as identifiers; the message is matched against a few fixed phrases to tell
// apart failures the API reports under a generic status, and is never kept.
type apiErrorBody struct {
	Error struct {
		Code    int           `json:"code"`
		Message string        `json:"message"`
		Status  string        `json:"status"`
		Details []errorDetail `json:"details"`
	} `json:"error"`
}

type errorDetail struct {
	Reason     string `json:"reason"`
	RetryDelay string `json:"retryDelay"`
	Violations []struct {
		QuotaID string `json:"quotaId"`
	} `json:"violations"`
}

func (b apiErrorBody) status() string {
	if safeStatusRE.MatchString(b.Error.Status) {
		return b.Error.Status
	}
	return ""
}

// reason is the first well-formed ErrorInfo reason.
func (b apiErrorBody) reason() string {
	for _, d := range b.Error.Details {
		if safeReasonRE.MatchString(d.Reason) {
			return d.Reason
		}
	}
	return ""
}

// dailyQuota is true when a QuotaFailure names a per-day quota: no wait of
// seconds cures it.
func (b apiErrorBody) dailyQuota() bool {
	for _, d := range b.Error.Details {
		for _, v := range d.Violations {
			if quotaIDRE.MatchString(v.QuotaID) && strings.Contains(strings.ToLower(v.QuotaID), "perday") {
				return true
			}
		}
	}
	return false
}

// retryDelay parses a RetryInfo retryDelay (a protobuf Duration such as "34s"
// or "7.5s").
func (b apiErrorBody) retryDelay() (time.Duration, bool) {
	for _, d := range b.Error.Details {
		v, ok := strings.CutSuffix(strings.TrimSpace(d.RetryDelay), "s")
		if !ok {
			continue
		}
		secs, err := strconv.ParseFloat(v, 64)
		if err != nil || !(secs >= 0) {
			continue
		}
		if secs > providers.MaxRetryAfter.Seconds() {
			return providers.MaxRetryAfter, true
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	return 0, false
}

// classify maps an HTTP status and the provider's status, reason and message to
// the ErrCode and ProviderErrorKind. retryable is true only for a refusal made
// before processing that a wait can cure: a 429 that is not a daily or billing
// quota, and 503. A 500 is not retried: the request may have been processed.
//
// The 429 discriminator, which the API does not state in one field: it is a
// quota failure (never retried) when a QuotaFailure names a per-day quota or the
// message speaks of billing, spend or prepayment credits; any other
// RESOURCE_EXHAUSTED is a rate limit, retried within the bound. Both labels are
// unverified until the live run (X-H02).
func classify(status int, st, reason, msg string, daily bool) (contracts.ErrCode, pv.ProviderErrorKind, bool) {
	lower := strings.ToLower(msg)
	switch {
	case reason == "API_KEY_INVALID" || reason == "API_KEY_EXPIRED" || reason == "API_KEY_MISSING" ||
		reason == "ACCESS_TOKEN_EXPIRED" || reason == "ACCESS_TOKEN_TYPE_UNSUPPORTED" || reason == "CREDENTIALS_MISSING" ||
		status == http.StatusUnauthorized || st == "UNAUTHENTICATED":
		// An invalid key is reported as 400 INVALID_ARGUMENT with reason
		// API_KEY_INVALID, not as 401.
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, false
	case status == http.StatusForbidden || st == "PERMISSION_DENIED" || st == "FAILED_PRECONDITION" ||
		strings.HasSuffix(reason, "_BLOCKED") || reason == "SERVICE_DISABLED" || reason == "ACCESS_TOKEN_SCOPE_INSUFFICIENT":
		// FAILED_PRECONDITION covers a project without billing and a region
		// without free-tier access.
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, false
	case status == http.StatusTooManyRequests || st == "RESOURCE_EXHAUSTED":
		if daily || strings.Contains(lower, "billing") || strings.Contains(lower, "spend") || strings.Contains(lower, "prepayment") {
			return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
		}
		return contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true
	case status == http.StatusNotFound || st == "NOT_FOUND":
		return contracts.ErrValidationFailed, pv.ProviderErrorKindModelNotFound, false
	case status == http.StatusRequestEntityTooLarge ||
		(status == http.StatusBadRequest && (strings.Contains(lower, "token count") || strings.Contains(lower, "exceeds the maximum number of tokens") || strings.Contains(lower, "too large"))):
		return contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, false
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout || st == "DEADLINE_EXCEEDED":
		return contracts.ErrTimeout, pv.ProviderErrorKindTimeout, false
	case status == http.StatusServiceUnavailable || st == "UNAVAILABLE":
		return contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true
	case status >= 500:
		return contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, false
	case status >= 400:
		return contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, false
	}
	// 1xx, 2xx other than 200, 3xx (redirects are refused): not an answer.
	return contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, false
}

// errorText is the fixed phrase for each kind.
func errorText(kind pv.ProviderErrorKind) string {
	switch kind {
	case pv.ProviderErrorKindAuthenticationFailed:
		return "the provider rejected the credential"
	case pv.ProviderErrorKindPermissionDenied:
		return "the provider denied the request for this credential or project"
	case pv.ProviderErrorKindRateLimited:
		return "the provider rate limit was reached"
	case pv.ProviderErrorKindQuotaExceeded:
		return "the provider project has no quota or credit left"
	case pv.ProviderErrorKindModelNotFound:
		return "the provider does not know the model or route"
	case pv.ProviderErrorKindContextTooLarge:
		return "the request is larger than the model accepts"
	case pv.ProviderErrorKindContentFiltered:
		return "the provider filtered the content"
	case pv.ProviderErrorKindTimeout:
		return "the provider timed out"
	case pv.ProviderErrorKindUnavailable:
		return "the provider is unavailable"
	case pv.ProviderErrorKindBadRequest:
		return "the provider rejected the request"
	}
	return "the provider sent an unexpected answer"
}

// mapHTTPError builds the typed error for a non-200 answer. The second result
// says whether a bounded retry may follow. status 0 means "read it from the
// body" (an error event inside a stream).
func mapHTTPError(status int, h http.Header, body []byte, now time.Time) (error, bool) {
	var eb apiErrorBody
	_ = json.NewDecoder(bytes.NewReader(body)).Decode(&eb) // an unreadable body only means no status/reason
	if status == 0 {
		status = eb.Error.Code
	}
	code, kind, retryable := classify(status, eb.status(), eb.reason(), eb.Error.Message, eb.dailyQuota())
	retryAfter, haveRetry := providers.ParseRetryAfter(h.Get("Retry-After"), now)
	if !haveRetry {
		retryAfter, haveRetry = eb.retryDelay()
	}
	var e *contracts.Error
	if code == contracts.ErrRateLimited {
		rl := providers.RateLimitInfo{Scope: "requests"}
		if haveRetry {
			rl.RetryAfter = retryAfter
		}
		e = external.ExtRateLimited(rl, kind)
	} else {
		e = apiErr(code, kind, true, errorText(kind))
		if retryable && haveRetry {
			e.WithDetails("retry_after_ms", retryAfter.Milliseconds())
		}
	}
	if status > 0 {
		e.WithDetails("http_status", status)
	}
	if s := eb.status(); s != "" {
		e.WithDetails("provider_type", s)
	}
	return e, retryable
}

// mapTransportError converts a failure of the connection or of the context.
func (c *Client) mapTransportError(ctx context.Context, err error) error {
	return external.ExtMapTransportError(ctx, ProviderName, err)
}

func (c *Client) wrapDecode(what string) error {
	return malformed(fmt.Sprintf("the %s is not the expected JSON document", what))
}
