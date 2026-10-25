package openapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons the adapter puts in Details["reason"] of the errors it raises itself.
// They are fixed strings: an error never carries text the application chose.
const (
	// ReasonOperationNotMapped: the package maps no operation for the call.
	ReasonOperationNotMapped = "operation_not_mapped"
	// ReasonUnknownEntityType: the reserved entity_type names a type the
	// capability declares no resolver for.
	ReasonUnknownEntityType = "unknown_entity_type"
	// ReasonRequestViolatesSpec: the request the adapter would send does not
	// satisfy the operation's description in the spec, so nothing was sent.
	ReasonRequestViolatesSpec = "request_violates_spec"
	// ReasonFieldNotAccepted: a field has no parameter and no request body to
	// travel in.
	ReasonFieldNotAccepted = "field_not_accepted"
	// ReasonValueNotEncodable: a value cannot be put in the place the operation
	// declares for it (a path segment that climbs, a list item with a comma).
	ReasonValueNotEncodable = "value_not_encodable"
	// ReasonRequestTooLarge: the request body is larger than the adapter sends.
	ReasonRequestTooLarge = "request_too_large"
	// ReasonResponseViolatesSpec: the reply does not satisfy the response the
	// spec describes for its status.
	ReasonResponseViolatesSpec = "response_violates_spec"
	// ReasonStatusNotDeclared: the application answered with a status the spec
	// does not describe for the operation.
	ReasonStatusNotDeclared = "status_not_declared"
	// ReasonResponseNotDescribed: the spec declares the status but describes no
	// JSON body for it, so the body cannot be checked and is not used.
	ReasonResponseNotDescribed = "response_not_described"
	// ReasonNotJSON: the reply is not application/json.
	ReasonNotJSON = "not_json"
	// ReasonRedirect: the application answered with a redirect. The adapter
	// never follows one.
	ReasonRedirect = "redirect_not_followed"
	// ReasonHTTPStatus: the application answered with a status the adapter
	// treats as a failure of the call (5xx, 429, 401, 403 without a result).
	ReasonHTTPStatus = "http_status"
	// ReasonCredentialUnavailable: the credential could not be read, so nothing
	// was sent.
	ReasonCredentialUnavailable = "credential_unavailable"
	// ReasonCredentialMalformed: the credential cannot be sent as a header
	// value, so nothing was sent.
	ReasonCredentialMalformed = "credential_malformed"
	// ReasonAdapterClosed: the adapter was closed.
	ReasonAdapterClosed = "adapter_closed"
	// ReasonNoReply: no usable reply arrived (the connection broke, the reply
	// was cut off or too large for the adapter to read).
	ReasonNoReply = "no_reply"
)

// typed builds a contracts.Error that names the transport.
func typed(code contracts.ErrCode, msg, reason string) *contracts.Error {
	e := contracts.New(code, msg).WithDetails("transport", Transport)
	if reason != "" {
		e.WithDetails("reason", reason)
	}
	return e
}

func responseTooLarge(limit int64) *contracts.Error {
	return typed(contracts.ErrSchemaInvalid, "reply is larger than the size limit", adapters.ReasonResponseTooLarge).
		WithDetails("limit_bytes", limit)
}

// credentialError is what the credential round tripper fails with. It carries
// no text of the provider or of the credential.
type credentialError struct{ reason string }

func (e *credentialError) Error() string {
	if e.reason == ReasonCredentialMalformed {
		return "the credential cannot be sent as a header value"
	}
	return "the credential of the application's endpoint could not be read"
}

// denialOf returns the egress refusal in err's chain, if there is one.
func denialOf(err error) *contracts.Error {
	var ce *contracts.Error
	for e := err; e != nil; {
		if !errors.As(e, &ce) || ce == nil {
			return nil
		}
		if egress.IsDenied(ce) {
			return ce
		}
		e = ce.Unwrap()
	}
	return nil
}

// notSent reports whether err proves that no byte of the request reached an
// application that could act on it: an egress refusal, a failed dial, a failed
// certificate check, a credential that could not be read.
func notSent(err error) bool {
	if denialOf(err) != nil {
		return true
	}
	var cred *credentialError
	if errors.As(err, &cred) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var cv *tls.CertificateVerificationError
	return errors.As(err, &cv)
}

// safeCause reduces a transport error to something that can be put in an error
// message and a log: the refusal of the egress gate, a network error with the
// URL taken off, a fixed description. Text an application chose never gets
// through, and neither does a URL (which can carry a query) or a header.
func safeCause(err error) error {
	if ce := denialOf(err); ce != nil {
		return ce
	}
	var cred *credentialError
	if errors.As(err, &cred) {
		return cred
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return egress.StripURL(op)
	}
	var cv *tls.CertificateVerificationError
	if errors.As(err, &cv) {
		return errors.New("the application's certificate was not accepted")
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("the connection closed")
	}
	return errors.New("the transport failed")
}

// sendFailure types the failure of an HTTP exchange that did not produce a
// reply. ctx is the caller's context, cctx the one the call ran under (the
// per-operation timeout and the adapter's own lifetime on top).
//
//   - the caller's cancellation or deadline is returned as the context's own
//     error, untyped: adapters.Wrap types it (timeout, or uncertain_outcome for
//     a Commit);
//   - a failure that proves nothing was delivered is unavailable, also for a
//     Commit, and keeps the egress refusal reachable in its chain;
//   - anything else may have been delivered: a Commit is uncertain_outcome (the
//     caller asks Status before it does anything else), the others are
//     unavailable.
func sendFailure(ctx, cctx context.Context, err error, commit, closed bool) error {
	switch {
	case ctx.Err() != nil:
		if commit {
			return adapters.UncertainOutcome(ctx.Err())
		}
		return ctx.Err()
	case closed:
		if commit {
			return adapters.UncertainOutcome(errors.New("the adapter was closed"))
		}
		return typed(contracts.ErrUnavailable, "the adapter was closed", ReasonAdapterClosed)
	case cctx.Err() != nil:
		e := fmt.Errorf("the call timed out: %w", cctx.Err())
		if commit {
			return adapters.UncertainOutcome(e)
		}
		return e
	case notSent(err):
		return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application could not be reached").WithDetails("transport", Transport)
	}
	if commit {
		return adapters.UncertainOutcome(safeCause(err))
	}
	return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application did not complete the call").
		WithDetails("transport", Transport).WithDetails("reason", ReasonNoReply)
}

// afterReply makes a failure that happened once a reply had started to arrive
// safe for a Commit: the application received the request, so it may have
// applied it, and no code that says "nothing happened" may be returned.
func afterReply(commit bool, err error) error {
	if err == nil || !commit {
		return err
	}
	if contracts.HasCode(err, contracts.ErrUncertainOutcome) {
		return err
	}
	return adapters.UncertainOutcome(err)
}
