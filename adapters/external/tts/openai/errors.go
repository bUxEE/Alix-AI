package openai

import (
	"net/http"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func apiErr(code contracts.ErrCode, kind pv.ProviderErrorKind, sent bool, msg string) *contracts.Error {
	return external.ExtAPIError(ProviderName, code, kind, sent, msg)
}

func mapHTTP(status int, _ http.Header, _ []byte) (error, bool) {
	switch {
	case status == http.StatusUnauthorized:
		return apiErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, "the provider rejected the API key"), false
	case status == http.StatusForbidden:
		return apiErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, true, "the provider denied the request for this key"), false
	case status == http.StatusTooManyRequests:
		return apiErr(contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, true, "the provider rate limit was reached"), true
	case status == http.StatusRequestEntityTooLarge:
		return apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, true, "the request is larger than the model accepts"), false
	case status == http.StatusServiceUnavailable:
		return apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, "the provider is unavailable"), true
	case status >= 500:
		return apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, "the provider is unavailable"), false
	case status >= 400:
		return apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, "the provider rejected the request"), false
	}
	return apiErr(contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, true, "the provider answered unexpectedly"), false
}
