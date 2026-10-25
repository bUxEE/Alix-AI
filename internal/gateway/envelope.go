package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"unicode/utf8"

	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// HeaderRequestID carries the request ID the gateway minted.
const HeaderRequestID = "X-Request-ID"

const (
	genericInternalMessage = "internal error"
	maxMessageLen          = 2048 // schemas/common/error: message maxLength
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// RequestID returns the ID the gateway minted for the request, "" outside a
// gateway request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// loggerFrom returns the request-scoped logger (already carrying the request
// ID), or a logger that discards.
func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.New(slog.DiscardHandler)
}

// statusForCode is the default HTTP status of an error code. Callers that know
// more pass their own status (WriteErrorStatus): the same code can be the
// client's fault on one route and an upstream fault on another.
func statusForCode(code contracts.ErrCode) int {
	switch code {
	case contracts.ErrUnauthenticated, contracts.ErrExpired, contracts.ErrReplayed, contracts.ErrScopeMissing:
		return http.StatusUnauthorized // the same mapping as identity.Middleware
	case contracts.ErrScopeViolation, contracts.ErrPermissionDenied, contracts.ErrStepUpRequired, contracts.ErrQuarantined:
		return http.StatusForbidden
	case contracts.ErrValidationFailed:
		return http.StatusBadRequest
	case contracts.ErrSchemaInvalid, contracts.ErrUnknownCapability, contracts.ErrMissingFields, contracts.ErrNoSupportedEvidence:
		return http.StatusUnprocessableEntity
	case contracts.ErrAmbiguous, contracts.ErrStale, contracts.ErrConflict:
		return http.StatusConflict
	case contracts.ErrConfirmationRequired:
		return http.StatusPreconditionRequired
	case contracts.ErrRateLimited:
		return http.StatusTooManyRequests
	case contracts.ErrUnavailable, contracts.ErrDegraded, contracts.ErrResourceExhausted:
		return http.StatusServiceUnavailable
	case contracts.ErrTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError // internal, uncertain_outcome, partial_failure and anything unknown
}

// problem is what goes on the wire for one error.
type problem struct {
	status int
	body   contracts.Error
	// canceled marks a request that was cancelled rather than one that failed:
	// the caller went away, or the server is stopping.
	canceled bool
}

// problemFor turns any error into its wire form. Only typed fields leave: the
// wrapped cause and the text of an untyped error stay in the server log
// (SEC-083), and the message of an internal fault is replaced by a fixed one.
func problemFor(err error) problem {
	if err == nil {
		err = contracts.New(contracts.ErrInternal, genericInternalMessage)
	}
	code := contracts.CodeOf(err)

	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return problem{status: http.StatusRequestEntityTooLarge, body: *contracts.New(contracts.ErrValidationFailed, "request body too large").
			WithDetails("limit", mbe.Limit)}
	case code == contracts.ErrInternal && errors.Is(err, context.Canceled):
		// A caller cancel is a plain error that CodeOf reports as internal. It
		// is not a fault of ours, and the client is usually gone already.
		return problem{status: http.StatusServiceUnavailable, canceled: true,
			body: *contracts.New(contracts.ErrUnavailable, "request canceled")}
	case code == contracts.ErrInternal && errors.Is(err, context.DeadlineExceeded):
		return problem{status: http.StatusGatewayTimeout, body: *contracts.New(contracts.ErrTimeout, "request timed out")}
	}

	var ce *contracts.Error
	if code == contracts.ErrInternal || !errors.As(err, &ce) || ce == nil {
		return problem{status: http.StatusInternalServerError, body: *contracts.New(contracts.ErrInternal, genericInternalMessage)}
	}
	msg := alog.RedactString(ce.Message)
	if len(msg) > maxMessageLen {
		msg = msg[:maxMessageLen]
		for len(msg) > 0 && !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	body := contracts.Error{Code: ce.Code, Message: msg, Retryable: ce.Code.Retryable(), Details: cleanDetails(ce.Details)}
	return problem{status: statusForCode(ce.Code), body: body}
}

var detailRedactor = alog.NewRedactor(nil)

// cleanDetails keeps the scalar and string-list details of an error, with
// anything credential-shaped removed: sensitive keys are masked and string
// values are scrubbed. Nested values are dropped rather than trusted.
func cleanDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if detailRedactor.SensitiveKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		switch x := v.(type) {
		case string:
			out[k] = alog.RedactString(x)
		case bool, int, int32, int64, uint, uint32, uint64, float32, float64:
			out[k] = x
		case []string:
			list := make([]string, len(x))
			for i, s := range x {
				list[i] = alog.RedactString(s)
			}
			out[k] = list
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WriteError writes err as the error envelope with the status its code implies.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	writeProblem(w, r, 0, err)
}

// WriteErrorStatus is WriteError with an explicit HTTP status, for a handler
// that knows the failure's origin (for example model output that fails its
// schema, which is 502 whatever the code's default status).
func WriteErrorStatus(w http.ResponseWriter, r *http.Request, status int, err error) {
	writeProblem(w, r, status, err)
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, err error) {
	p := problemFor(err)
	if status != 0 {
		p.status = status
	}
	ctx := r.Context()
	id := RequestID(ctx)
	if id != "" {
		if p.body.Details == nil {
			p.body.Details = map[string]any{}
		}
		p.body.Details["request_id"] = id
	}

	log := loggerFrom(ctx)
	attrs := []any{slog.String("code", string(p.body.Code)), slog.Int("status", p.status)}
	if err != nil {
		attrs = append(attrs, slog.String("err_text", err.Error()))
	}
	switch {
	case p.canceled && ctx.Err() != nil:
		log.Info("request canceled by the caller", attrs...)
	case p.status >= 500:
		log.Error("request failed", attrs...)
	default:
		log.Info("request refused", attrs...)
	}

	if sw, ok := w.(*statusWriter); ok && sw.wrote {
		return // the response has begun; nothing more can be said to the client
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if p.status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", "Bearer")
	}
	if p.body.Retryable {
		h.Set("Retry-After", "1")
	}
	w.WriteHeader(p.status)
	_ = json.NewEncoder(w).Encode(p.body)
}

// writeJSON writes a success body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(contracts.New(contracts.ErrInternal, genericInternalMessage))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
