package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

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
// repeats an Authorization header must not reach a log. The messages are fixed
// phrases.

func apiErr(code contracts.ErrCode, kind pv.ProviderErrorKind, sent bool, msg string) *contracts.Error {
	return external.ExtAPIError(ProviderName, code, kind, sent, msg)
}

func malformed(msg string) *contracts.Error { return external.ExtMalformed(ProviderName, msg) }

var safeCodeRE = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// apiErrorBody is the error envelope: {"error": {"message", "type", "param",
// "code"}}. Only type and code are read, and only when they have the shape of
// an identifier.
type apiErrorBody struct {
	Error struct {
		Type string `json:"type"`
		Code any    `json:"code"`
	} `json:"error"`
}

func (b apiErrorBody) code() string {
	if s, ok := b.Error.Code.(string); ok && safeCodeRE.MatchString(s) {
		return s
	}
	return ""
}

func (b apiErrorBody) typ() string {
	if safeCodeRE.MatchString(b.Error.Type) {
		return b.Error.Type
	}
	return ""
}

// classify maps an HTTP status and the provider's error type and code to the
// ErrCode and ProviderErrorKind. retryable is true only for a refusal made
// before processing that a wait can cure: 429 rate_limit (never a quota
// failure) and 503 overload.
func classify(status int, typ, code string) (contracts.ErrCode, pv.ProviderErrorKind, bool) {
	quota := code == "insufficient_quota" || typ == "insufficient_quota"
	switch {
	case status == http.StatusUnauthorized:
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, false
	case status == http.StatusForbidden:
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, false
	case status == http.StatusTooManyRequests && quota:
		return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
	case status == http.StatusTooManyRequests:
		return contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true
	case status == http.StatusNotFound || code == "model_not_found":
		return contracts.ErrValidationFailed, pv.ProviderErrorKindModelNotFound, false
	case status == http.StatusRequestEntityTooLarge || code == "context_length_exceeded" || code == "string_above_max_length":
		return contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, false
	case code == "content_filter" || code == "content_policy_violation":
		return contracts.ErrValidationFailed, pv.ProviderErrorKindContentFiltered, false
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return contracts.ErrTimeout, pv.ProviderErrorKindTimeout, false
	case status == http.StatusServiceUnavailable:
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
		return "the provider rejected the API key"
	case pv.ProviderErrorKindPermissionDenied:
		return "the provider denied the request for this key"
	case pv.ProviderErrorKindRateLimited:
		return "the provider rate limit was reached"
	case pv.ProviderErrorKindQuotaExceeded:
		return "the provider account has no quota left"
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
// says whether a bounded retry may follow.
func mapHTTPError(status int, h http.Header, body []byte, info *providers.RateLimitInfo, now time.Time) (error, bool) {
	var eb apiErrorBody
	_ = json.NewDecoder(bytes.NewReader(body)).Decode(&eb) // an unreadable body only means no type/code
	code, kind, retryable := classify(status, eb.typ(), eb.code())
	var e *contracts.Error
	if code == contracts.ErrRateLimited {
		rl := providers.RateLimitInfo{Scope: "requests"}
		if info != nil {
			rl = *info
			if rl.Scope == "" {
				rl.Scope = "requests"
			}
		}
		if ra, ok := providers.ParseRetryAfter(h.Get("Retry-After"), now); ok {
			rl.RetryAfter = ra
		}
		e = external.ExtRateLimited(rl, kind)
	} else {
		e = apiErr(code, kind, true, errorText(kind))
		if status == http.StatusServiceUnavailable {
			if ra, ok := providers.ParseRetryAfter(h.Get("Retry-After"), now); ok {
				e.WithDetails("retry_after_ms", ra.Milliseconds())
			}
		}
	}
	e.WithDetails("http_status", status)
	if c := eb.code(); c != "" {
		e.WithDetails("provider_code", c)
	}
	return e, retryable
}

// mapFailureCode maps the error code of a failed response object or stream
// event ("server_error", "rate_limit_exceeded", ...) to a typed error. The
// message is never read.
func mapFailureCode(code string) error {
	if !safeCodeRE.MatchString(code) {
		code = ""
	}
	var e *contracts.Error
	switch code {
	case "rate_limit_exceeded":
		e = apiErr(contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, errorText(pv.ProviderErrorKindRateLimited))
	case "insufficient_quota":
		e = apiErr(contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, true, errorText(pv.ProviderErrorKindQuotaExceeded))
	case "context_length_exceeded":
		e = apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, true, errorText(pv.ProviderErrorKindContextTooLarge))
	case "content_filter":
		e = apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindContentFiltered, true, errorText(pv.ProviderErrorKindContentFiltered))
	case "invalid_prompt", "invalid_request_error":
		e = apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, errorText(pv.ProviderErrorKindBadRequest))
	default:
		e = apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, errorText(pv.ProviderErrorKindUnavailable))
	}
	if code != "" {
		e.WithDetails("provider_code", code)
	}
	return e
}

// mapTransportError converts a failure of the connection or of the context.
func (c *Client) mapTransportError(ctx context.Context, err error) error {
	return external.ExtMapTransportError(ctx, ProviderName, err)
}

// Rate-limit headers ------------------------------------------------------

// rateInfo reads the documented x-ratelimit-* headers. It returns nil when the
// provider sent none. When a limit is exhausted, the retry hint comes from its
// reset header (a duration such as "1s" or "6m0s").
func rateInfo(h http.Header, now time.Time) *providers.RateLimitInfo {
	var info providers.RateLimitInfo
	seen := false
	atoi := func(name string) *int {
		v := h.Get(name)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil
		}
		seen = true
		return &n
	}
	info.RemainingRequests = atoi("X-Ratelimit-Remaining-Requests")
	info.RemainingToks = atoi("X-Ratelimit-Remaining-Tokens")
	reset := func(name string) time.Duration {
		d, err := time.ParseDuration(h.Get(name))
		if err != nil || d < 0 {
			return 0
		}
		if d > providers.MaxRetryAfter {
			d = providers.MaxRetryAfter
		}
		return d
	}
	switch {
	case info.RemainingRequests != nil && *info.RemainingRequests == 0:
		info.Scope, info.RetryAfter = "requests", reset("X-Ratelimit-Reset-Requests")
	case info.RemainingToks != nil && *info.RemainingToks == 0:
		info.Scope, info.RetryAfter = "toks", reset("X-Ratelimit-Reset-Tokens")
	}
	if ra, ok := providers.ParseRetryAfter(h.Get("Retry-After"), now); ok {
		info.RetryAfter, seen = ra, true
	}
	if !seen {
		return nil
	}
	return &info
}

func (c *Client) wrapDecode(what string) error {
	return malformed(fmt.Sprintf("the %s is not the expected JSON document", what))
}
