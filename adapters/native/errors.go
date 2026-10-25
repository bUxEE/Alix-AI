package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons the adapter puts in Details["reason"] of the errors it raises itself.
// They are fixed strings: an error never carries text the host chose.
const (
	// ReasonOperationNotMapped: the package maps no method for the call.
	ReasonOperationNotMapped = "operation_not_mapped"
	// ReasonOperationNotOffered: the package maps a method the bridge does not
	// declare. The bridge answered before any handler ran, so nothing was applied.
	ReasonOperationNotOffered = "operation_not_offered"
	// ReasonUnknownEntityType: the reserved entity_type names a type the
	// capability declares no resolver for.
	ReasonUnknownEntityType = "unknown_entity_type"
	// ReasonPeerAuthFailed: the process at the bridge address did not prove it
	// holds the application key, or refused this adapter's proof. Nothing of the
	// call was sent.
	ReasonPeerAuthFailed = "peer_authentication_failed"
	// ReasonPeerRefused: the operating system's account of the bridge process
	// (peer credentials) is not acceptable. Nothing was sent.
	ReasonPeerRefused = "peer_credentials_refused"
	// ReasonSocketPermissions: the socket or its directory is accessible to other
	// users. Nothing was sent.
	ReasonSocketPermissions = "socket_permissions"
	// ReasonRequestTooLarge: the request frame would exceed the limit. Nothing was
	// sent.
	ReasonRequestTooLarge = "request_too_large"
	// ReasonProtocol: the reply is not a JSON-RPC response to this call.
	ReasonProtocol = "protocol_violation"
	// ReasonHostError: the bridge answered with a JSON-RPC error instead of a
	// result.
	ReasonHostError = "host_error"
	// ReasonNoReply: no usable reply arrived (the connection broke).
	ReasonNoReply = "no_reply"
	// ReasonAdapterClosed: the adapter was closed.
	ReasonAdapterClosed = "adapter_closed"
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

// refusal is an error that proves nothing of the call was sent: unavailable,
// with a fixed reason, also for a Commit.
func refusal(reason, msg string) *contracts.Error {
	return typed(contracts.ErrUnavailable, msg, reason)
}

// hostError types a JSON-RPC error reply.
func hostError(e *rpcError) *contracts.Error {
	if e.Code == codeMethodNotFound {
		return typed(contracts.ErrUnknownCapability, "the bridge does not declare the method this operation is mapped to", ReasonOperationNotOffered)
	}
	return typed(contracts.ErrUnavailable, "the host could not complete the call", ReasonHostError).WithDetails("rpc_code", e.Code)
}

func reasonOf(err error) string {
	for err != nil {
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce == nil {
			return ""
		}
		if r, ok := ce.Details["reason"].(string); ok {
			return r
		}
		err = ce.Unwrap()
	}
	return ""
}

// afterReply makes a failure that happened once the request was sent safe for a
// Commit: the host received it, so it may have applied it, and no code that says
// "nothing happened" may be returned. The one exception is a bridge that does not
// declare the method: it answered before any handler ran.
func afterReply(commit bool, err error) error {
	if err == nil || !commit {
		return err
	}
	if contracts.HasCode(err, contracts.ErrUncertainOutcome) || reasonOf(err) == ReasonOperationNotOffered {
		return err
	}
	return adapters.UncertainOutcome(err)
}

// safeCause reduces a transport error to something that can be put in an error
// message and a log: a fixed description. Text a peer chose never gets through.
func safeCause(err error) error {
	var op *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("the connection closed")
	case errors.As(err, &op):
		return egress.StripURL(op)
	}
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil && egress.IsDenied(ce) {
		return ce
	}
	return errors.New("the transport failed")
}

// failure types an error of one exchange. ctx is the caller's context and cctx
// the one the call ran under (the binding's timeout and the adapter's own
// lifetime on top); sent says whether the request frame was written.
//
//   - the caller's cancellation or deadline is returned as the context's own
//     error, untyped: adapters.Wrap types it (timeout, or uncertain_outcome for a
//     Commit). Once a Commit was sent it is uncertain_outcome here already;
//   - a failure before the request was sent proves nothing was delivered: it is
//     unavailable, also for a Commit;
//   - anything after may have been delivered: a Commit is uncertain_outcome (the
//     caller asks Status before it does anything else), the others unavailable.
func (a *Adapter) failure(ctx, cctx context.Context, err error, sent, commit bool) error {
	switch {
	case ctx.Err() != nil:
		if commit && sent {
			return adapters.UncertainOutcome(ctx.Err())
		}
		return ctx.Err()
	case a.closeCtx.Err() != nil:
		if commit && sent {
			return adapters.UncertainOutcome(errors.New("the adapter was closed"))
		}
		return refusal(ReasonAdapterClosed, "the adapter was closed")
	case cctx.Err() != nil:
		e := fmt.Errorf("the call timed out: %w", cctx.Err())
		if commit && sent {
			return adapters.UncertainOutcome(e)
		}
		return e
	}
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		if sent {
			return afterReply(commit, ce)
		}
		return ce
	}
	switch {
	case errors.Is(err, errPeerAuth):
		return refusal(ReasonPeerAuthFailed, "the bridge did not authenticate, or refused this adapter")
	case !sent:
		return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application could not be reached").WithDetails("transport", Transport)
	case commit:
		return adapters.UncertainOutcome(safeCause(err))
	}
	return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application did not complete the call").
		WithDetails("transport", Transport).WithDetails("reason", ReasonNoReply)
}
