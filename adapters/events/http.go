package events

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Handler is the webhook intake: POST a delivery as application/json. It reads
// the delivery, and nothing else, from the request: the application, tenant and
// workspace an event is for come from its signed payload and never from the URL,
// a header or the connection, so a request cannot name or widen a scope. The
// handler authenticates nothing itself (the signature does) and starts no
// listener; the composition root mounts it on a route.
//
// Responses: 200 with a Receipt for an applied or duplicate event; 400 for a
// malformed delivery or scope; 401 for an event that is unsigned, badly signed,
// signed by an unknown or revoked key or expired; 403 for an event for another
// application or an unaccepted scope; 405, 413 and 415 for the wrong method, a
// body over MaxBodyBytes and the wrong content type; 503 when the event could
// not be applied and should be delivered again.
func (i *Intake) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodPost {
			h.Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, contracts.New(contracts.ErrValidationFailed, "events: events are delivered with POST"))
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, contracts.New(contracts.ErrValidationFailed, "events: a delivery is application/json"))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, i.cfg.MaxBodyBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, contracts.New(contracts.ErrSchemaInvalid, "events: the delivery is too large"))
				return
			}
			writeError(w, http.StatusBadRequest, contracts.New(contracts.ErrSchemaInvalid, "events: the delivery could not be read"))
			return
		}
		rc, err := i.Deliver(r.Context(), body)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(rc)
	})
}

// statusFor maps a failure to an HTTP status. A failure it does not know is a
// 500: a sender must not read it as anything but "not applied".
func statusFor(err error) int {
	switch contracts.CodeOf(err) {
	case contracts.ErrUnauthenticated, contracts.ErrExpired:
		return http.StatusUnauthorized
	case contracts.ErrPermissionDenied, contracts.ErrScopeViolation:
		return http.StatusForbidden
	case contracts.ErrScopeMissing, contracts.ErrSchemaInvalid, contracts.ErrValidationFailed:
		return http.StatusBadRequest
	case contracts.ErrUnavailable, contracts.ErrTimeout, contracts.ErrRateLimited, contracts.ErrResourceExhausted, contracts.ErrDegraded:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// writeError writes the typed error as JSON. A refused signature is described by
// trust's closed vocabulary of reasons and nothing else, so a sender can debug
// its key and its clock without the response carrying anything about the store.
func writeError(w http.ResponseWriter, status int, err error) {
	body := contracts.Error{Code: contracts.CodeOf(err)}
	var ve *trust.VerifyError
	var ce *contracts.Error
	switch {
	case errors.As(err, &ve):
		body.Message = "events: the event signature was not accepted"
		if len(ve.Failures) > 0 {
			body.Details = map[string]any{"reason": string(ve.Failures[0].Reason)}
		}
	case errors.As(err, &ce):
		body.Message = ce.Message
		body.Details = ce.Details
	default:
		body.Code = contracts.ErrInternal
		body.Message = "events: the event could not be processed"
	}
	if body.Code == "" {
		body.Code = contracts.ErrInternal
	}
	body.Retryable = body.Code.Retryable()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
