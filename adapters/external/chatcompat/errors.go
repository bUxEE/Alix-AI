package chatcompat

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

func apiErr(provider string, code contracts.ErrCode, kind pv.ProviderErrorKind, sent bool, msg string) *contracts.Error {
	return external.ExtAPIError(provider, code, kind, sent, msg)
}

func malformed(provider, msg string) *contracts.Error { return external.ExtMalformed(provider, msg) }

var safeCodeRE = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

type apiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
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

func tooLongText(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context length") || strings.Contains(m, "context window") || strings.Contains(m, "maximum context") ||
		strings.Contains(m, "context_length_exceeded")
}

func classify(status int, typ, code, msg string) (contracts.ErrCode, pv.ProviderErrorKind, bool) {
	quota := code == "insufficient_quota" || typ == "insufficient_quota" || code == "billing_hard_limit_reached"
	switch {
	case status == http.StatusUnauthorized:
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, false
	case status == http.StatusPaymentRequired:
		return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
	case status == http.StatusForbidden:
		return contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, false
	case status == http.StatusTooManyRequests && quota:
		return contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, false
	case status == http.StatusTooManyRequests:
		return contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true
	case status == http.StatusNotFound || code == "model_not_found":
		return contracts.ErrValidationFailed, pv.ProviderErrorKindModelNotFound, false
	case status == http.StatusRequestEntityTooLarge || code == "context_length_exceeded" || (status == http.StatusBadRequest && tooLongText(msg)):
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
	return contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, false
}

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

func mapHTTPError(provider string, status int, h http.Header, body []byte, info *providers.RateLimitInfo, now time.Time) (error, bool) {
	var eb apiErrorBody
	_ = json.NewDecoder(bytes.NewReader(body)).Decode(&eb)
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
		e = apiErr(provider, code, kind, true, errorText(kind))
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

func mapFailureCode(provider, code string) error {
	if !safeCodeRE.MatchString(code) {
		code = ""
	}
	var e *contracts.Error
	switch code {
	case "rate_limit_exceeded":
		e = apiErr(provider, contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, errorText(pv.ProviderErrorKindRateLimited))
	case "insufficient_quota":
		e = apiErr(provider, contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, true, errorText(pv.ProviderErrorKindQuotaExceeded))
	case "context_length_exceeded":
		e = apiErr(provider, contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, true, errorText(pv.ProviderErrorKindContextTooLarge))
	case "content_filter":
		e = apiErr(provider, contracts.ErrValidationFailed, pv.ProviderErrorKindContentFiltered, true, errorText(pv.ProviderErrorKindContentFiltered))
	case "invalid_request_error", "invalid_prompt":
		e = apiErr(provider, contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, errorText(pv.ProviderErrorKindBadRequest))
	default:
		e = apiErr(provider, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, errorText(pv.ProviderErrorKindUnavailable))
	}
	if code != "" {
		e.WithDetails("provider_code", code)
	}
	return e
}

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
		v := h.Get(name)
		d, err := time.ParseDuration(v)
		if err != nil {
			secs, ferr := strconv.ParseFloat(v, 64)
			if ferr != nil || secs < 0 || secs > 1e6 {
				return 0
			}
			d = time.Duration(secs * float64(time.Second))
		}
		if d < 0 {
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
	return malformed(c.provider, fmt.Sprintf("the %s is not the expected JSON document", what))
}

func (c *Client) mapTransportError(ctx context.Context, err error) error {
	return external.ExtMapTransportError(ctx, c.provider, err)
}
