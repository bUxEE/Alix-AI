package anthropic

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

var safeCodeRE = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// apiErrorBody is the error envelope: {"type":"error","error":{"type",
// "message","details":{"error_code"}},"request_id"}. Only type and
// details.error_code are read as identifiers; the message is matched against a
// few fixed phrases to tell apart failures the API reports under a generic
// type, and is never kept.
type apiErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Details struct {
			ErrorCode string `json:"error_code"`
		} `json:"details"`
	} `json:"error"`
}

func (b apiErrorBody) typ() string {
	if safeCodeRE.MatchString(b.Error.Type) {
		return b.Error.Type
	}
	return ""
}

func (b apiErrorBody) code() string {
	if safeCodeRE.MatchString(b.Error.Details.ErrorCode) {
		return b.Error.Details.ErrorCode
	}
	return ""
}

// spendLimitCode is error.details.error_code of the 429 sent once the tier's
// monthly spend cap is reached. No wait cures it and it has no retry-after.
const spendLimitCode = "enforced_spend_limit_reached"

// classify maps an HTTP status and the provider's error type, code and message
// to the ErrCode and ProviderErrorKind. retryable is true only for a refusal
// made before processing that a wait can cure: 429 rate_limit_error (never the
// spend cap) and 503 / 529 overload.
func classify(status int, typ, code, msg string) (contracts.ErrCode, pv.ProviderErrorKind, bool) {
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusUnauthorized || typ == "authentication_error":
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, false
	case status == http.StatusPaymentRequired || typ == "billing_error":
		return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
	case status == http.StatusForbidden || typ == "permission_error":
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, false
	case status == http.StatusTooManyRequests && code == spendLimitCode:
		return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
	case status == http.StatusTooManyRequests:
		return contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true
	case status == http.StatusNotFound || typ == "not_found_error":
		return contracts.ErrValidationFailed, pv.ProviderErrorKindModelNotFound, false
	case status == http.StatusRequestEntityTooLarge || typ == "request_too_large" ||
		(status == http.StatusBadRequest && (strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "exceed the context window"))):
		return contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, false
	case status == http.StatusBadRequest && strings.Contains(lower, "reached your specified"):
		// A spend limit the operator set in the Console is reported as a
		// plain 400: it is a quota failure, not a malformed request.
		return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout || typ == "timeout_error":
		return contracts.ErrTimeout, pv.ProviderErrorKindTimeout, false
	case status == http.StatusServiceUnavailable || status == 529 || typ == "overloaded_error":
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
		return "the provider account has no quota or credit left"
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
	code, kind, retryable := classify(status, eb.typ(), eb.code(), eb.Error.Message)
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
		if retryable {
			if ra, ok := providers.ParseRetryAfter(h.Get("Retry-After"), now); ok {
				e.WithDetails("retry_after_ms", ra.Milliseconds())
			}
		}
	}
	e.WithDetails("http_status", status)
	if t := eb.typ(); t != "" {
		e.WithDetails("provider_type", t)
	}
	return e, retryable
}

// mapErrorType maps the error type of an error event inside a stream (the
// stream was already answered 200) to a typed error. The message is never read.
func mapErrorType(typ string) error {
	if !safeCodeRE.MatchString(typ) {
		typ = ""
	}
	var e *contracts.Error
	switch typ {
	case "rate_limit_error":
		e = apiErr(contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, errorText(pv.ProviderErrorKindRateLimited))
	case "billing_error":
		e = apiErr(contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, true, errorText(pv.ProviderErrorKindQuotaExceeded))
	case "authentication_error":
		e = apiErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, errorText(pv.ProviderErrorKindAuthenticationFailed))
	case "permission_error":
		e = apiErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, true, errorText(pv.ProviderErrorKindPermissionDenied))
	case "request_too_large":
		e = apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, true, errorText(pv.ProviderErrorKindContextTooLarge))
	case "invalid_request_error", "not_found_error":
		e = apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, errorText(pv.ProviderErrorKindBadRequest))
	case "timeout_error":
		e = apiErr(contracts.ErrTimeout, pv.ProviderErrorKindTimeout, true, errorText(pv.ProviderErrorKindTimeout))
	default: // overloaded_error, api_error and anything new
		e = apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, errorText(pv.ProviderErrorKindUnavailable))
	}
	if typ != "" {
		e.WithDetails("provider_type", typ)
	}
	return e
}

// mapTransportError converts a failure of the connection or of the context.
func (c *Client) mapTransportError(ctx context.Context, err error) error {
	return external.ExtMapTransportError(ctx, ProviderName, err)
}

// Rate-limit headers ------------------------------------------------------

// rateInfo reads the documented anthropic-ratelimit-* headers. It returns nil
// when the provider sent none. Reset values are RFC 3339 timestamps. When a
// limit is exhausted, the retry hint is the time until its reset.
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
	until := func(name string) time.Duration {
		t, err := time.Parse(time.RFC3339, h.Get(name))
		if err != nil {
			return 0
		}
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		if d > providers.MaxRetryAfter {
			d = providers.MaxRetryAfter
		}
		return d
	}
	info.RemainingRequests = atoi("Anthropic-Ratelimit-Requests-Remaining")
	// The combined token headers show the most restrictive limit in effect;
	// otherwise the tighter of the input and output buckets is reported.
	toks := atoi("Anthropic-Ratelimit-Tokens-Remaining")
	in := atoi("Anthropic-Ratelimit-Input-Tokens-Remaining")
	out := atoi("Anthropic-Ratelimit-Output-Tokens-Remaining")
	resetName := "Anthropic-Ratelimit-Tokens-Reset"
	switch {
	case toks != nil:
		info.RemainingToks = toks
	case in != nil && (out == nil || *in <= *out):
		info.RemainingToks, resetName = in, "Anthropic-Ratelimit-Input-Tokens-Reset"
	case out != nil:
		info.RemainingToks, resetName = out, "Anthropic-Ratelimit-Output-Tokens-Reset"
	}
	switch {
	case info.RemainingRequests != nil && *info.RemainingRequests == 0:
		info.Scope, info.RetryAfter = "requests", until("Anthropic-Ratelimit-Requests-Reset")
	case info.RemainingToks != nil && *info.RemainingToks == 0:
		info.Scope, info.RetryAfter = "toks", until(resetName)
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
