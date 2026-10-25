package sdkgo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Every failure the SDK reports is typed by a contracts.ErrCode. A failure that never reached the server
// (a malformed argument, a token provider that failed, an unreadable response) is a *contracts.Error; a
// failure the server answered is an *APIError, which unwraps to one, so contracts.CodeOf, contracts.HasCode
// and errors.Is(err, contracts.New(code, "")) work on both. A cancelled or expired caller context is
// returned as the context's own error.

// APIError is a non-2xx answer of the server, or a transport failure of a request.
type APIError struct {
	// Err carries the code, message, retryable flag and details of the server's error document.
	Err *contracts.Error
	// Status is the HTTP status; 0 for a failure before a response.
	Status int
	// RequestID is the X-Request-ID the gateway minted, for support and log correlation.
	RequestID string
	// RetryAfter is the server's Retry-After, zero when absent.
	RetryAfter time.Duration
	// Receipt is the durable record that came beside the error of a confirmation that did not commit.
	Receipt *actions.ActionReceipt
	// Preparation is the preparation as it stood when a confirmation did not commit.
	Preparation *actions.PreparedAction
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d)", e.Err.Error(), e.Status)
	}
	return e.Err.Error()
}

// Unwrap returns the *contracts.Error.
func (e *APIError) Unwrap() error { return e.Err }

// Code is the ErrCode of the failure.
func (e *APIError) Code() contracts.ErrCode { return e.Err.Code }

// IsRetryable reports whether sending the same request again, after a delay, can reasonably give a
// different answer. An uncertain_outcome is never retryable: read the state first.
func IsRetryable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return api.Err.Retryable
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Retryable
	}
	return false
}

// localError is a failure that never reached the server.
func localError(code contracts.ErrCode, message string, details ...any) *contracts.Error {
	e := contracts.New(code, message)
	for i := 0; i+1 < len(details); i += 2 {
		if k, ok := details[i].(string); ok {
			e.WithDetails(k, details[i+1])
		}
	}
	return e
}

func invalidArgument(argument, reason string) *contracts.Error {
	return localError(contracts.ErrValidationFailed, argument+": "+reason, "argument", argument)
}

// unexpectedShape is a success status whose body is not what the document says.
func unexpectedShape(op Operation, cause error) *contracts.Error {
	e := contracts.Wrap(contracts.ErrInternal, cause, "the server's "+op.ID+" response has an unexpected shape")
	return e.WithDetails("operation", op.ID).WithDetails("reason", "unexpected_shape")
}

// malformedTurn is a turn response (stream or JSON) that breaks the API's promises about it.
func malformedTurn(reason string, extra ...any) *contracts.Error {
	return localError(contracts.ErrInternal, "the server's turn response is malformed: "+reason,
		append([]any{"reason", "malformed_turn"}, extra...)...)
}

// codeForStatus is the code to report for an HTTP failure whose body is not an AlixAI error document.
func codeForStatus(status int) contracts.ErrCode {
	switch status {
	case 400, 413, 415, 422:
		return contracts.ErrValidationFailed
	case 401:
		return contracts.ErrUnauthenticated
	case 403:
		return contracts.ErrPermissionDenied
	case 409:
		return contracts.ErrConflict
	case 410:
		return contracts.ErrExpired
	case 429:
		return contracts.ErrRateLimited
	case 502, 503:
		return contracts.ErrUnavailable
	case 504:
		return contracts.ErrTimeout
	default:
		return contracts.ErrInternal
	}
}

// parseRetryAfter reads delta-seconds or an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if n, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

const maxErrorBody = 64 * 1024

// errorFromResponse turns a non-2xx response into an *APIError. The body is the bare Error document or, for
// a confirmation whose commit reached the host and did not succeed, the ActionOutcome with the receipt beside
// an `error` member. Anything else (an HTML page from a proxy) becomes a typed fallback chosen by the status:
// a parse failure never escapes.
func errorFromResponse(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if len(body) > maxErrorBody {
		body = nil
	}
	return errorFromBytes(resp, body)
}

// errorFromBytes is errorFromResponse for a body that has already been read.
func errorFromBytes(resp *http.Response, body []byte) *APIError {
	out := &APIError{
		Status:     resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-ID"),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) == nil {
		if raw, ok := members["error"]; ok { // an ActionOutcome: the receipt is the record, `error` says why
			if e := decodeErrorBody(raw); e != nil {
				out.Err = e
				out.Receipt = decodeOptional[actions.ActionReceipt](members["receipt"], out, "receipt")
				out.Preparation = decodeOptional[actions.PreparedAction](members["preparation"], out, "preparation")
				return out
			}
		} else if e := decodeErrorBody(body); e != nil {
			out.Err = e
			return out
		}
	}
	out.Err = contracts.New(codeForStatus(resp.StatusCode),
		fmt.Sprintf("the server answered %d without an AlixAI error document", resp.StatusCode)).
		WithDetails("reason", "non_conforming_error_body")
	return out
}

// decodeOptional decodes the receipt or the preparation beside an error. One that cannot be read is
// reported in the error's details rather than dropped silently.
func decodeOptional[T any](raw json.RawMessage, out *APIError, name string) *T {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		if out.Err != nil {
			out.Err.WithDetails(name+"_unreadable", true)
		}
		return nil
	}
	return &v
}

// decodeErrorBody reads an Error document: a defined code, a message and the retryable flag are required.
func decodeErrorBody(raw []byte) *contracts.Error {
	var doc struct {
		Code      contracts.ErrCode `json:"code"`
		Message   *string           `json:"message"`
		Retryable *bool             `json:"retryable"`
		Details   map[string]any    `json:"details"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || !doc.Code.Valid() || doc.Message == nil || doc.Retryable == nil {
		return nil
	}
	e := contracts.New(doc.Code, *doc.Message)
	e.Retryable = *doc.Retryable
	e.Details = doc.Details
	return e
}
