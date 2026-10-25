package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons the adapter puts in Details["reason"] of the errors it raises itself.
const (
	// ReasonTextOnlyResult: the tool answered with text and no structured
	// content. Text is never a fact (master plan 4.5 check 2, SEC-008).
	ReasonTextOnlyResult = "text_only_result"
	// ReasonToolError: the tool reported an error (isError) instead of a result.
	ReasonToolError = "tool_error"
	// ReasonInputRequired: the server asked the client for something (a
	// sampling or elicitation round trip). The adapter offers neither.
	ReasonInputRequired = "input_required_unsupported"
	// ReasonStructuredNotObject: the structured content is not a JSON object.
	ReasonStructuredNotObject = "structured_content_not_object"
	// ReasonToolSchemaDrift: a tool changed after it was approved.
	ReasonToolSchemaDrift = "tool_schema_drift"
	// ReasonOperationNotMapped: the package maps no tool for the operation.
	ReasonOperationNotMapped = "operation_not_mapped"
	// ReasonOperationNotOffered: the package maps a tool the server does not
	// list.
	ReasonOperationNotOffered = "operation_not_offered"
	// ReasonUnknownEntityType: the reserved entity_type names a type the
	// capability does not declare.
	ReasonUnknownEntityType = "unknown_entity_type"
	// ReasonTooManyTools: the server lists more tools than the adapter reads.
	ReasonTooManyTools = "too_many_tools"
)

// route is one mapped operation: the endpoint, the tool, its timeout and its
// argument renames.
type route struct {
	ep      *endpointConn
	tool    string
	timeout time.Duration
	args    map[string]string
}

// reply is the usable part of a tool result.
type reply struct {
	structured []byte
	echoedKey  string
}

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

func quarantinedErr(d Drift) *contracts.Error {
	e := typed(contracts.ErrQuarantined,
		"a tool of the application changed since it was approved; calls stay refused until the application is audited again", ReasonToolSchemaDrift).
		WithDetails("endpoint", d.Endpoint).WithDetails("drift", string(d.Kind))
	if d.Tool != "" {
		e.WithDetails("tool", d.Tool)
	}
	return e
}

func notOfferedErr(tool string) *contracts.Error {
	return typed(contracts.ErrUnknownCapability, "the application does not offer the tool this operation is mapped to", ReasonOperationNotOffered).
		WithDetails("tool", tool)
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

// notSent reports whether err proves that the request never left the process or
// never reached a server that could act on it: an egress refusal, a failed dial,
// a failed certificate check or a typed unavailable raised before the send.
func notSent(err error) bool {
	if denialOf(err) != nil {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var cv *tls.CertificateVerificationError
	if errors.As(err, &cv) {
		return true
	}
	var ce *contracts.Error
	return errors.As(err, &ce) && ce != nil && ce.Code == contracts.ErrUnavailable
}

// safeCause reduces a transport error to something that can be put in an error
// message: a refusal of the egress gate, a network error with the URL taken off,
// a fixed description. Text a server chose (JSON-RPC error messages, session
// identifiers in the SDK's wrapping) never gets through.
func safeCause(err error) error {
	if ce := denialOf(err); ce != nil {
		return ce
	}
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return fmt.Errorf("the server answered with JSON-RPC error %d", je.Code)
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return egress.StripURL(op)
	}
	var cv *tls.CertificateVerificationError
	if errors.As(err, &cv) {
		return errors.New("the server's certificate was not accepted")
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, mcp.ErrConnectionClosed):
		return errors.New("the connection closed")
	}
	return errors.New("the transport failed")
}

// unavailable is the error for a failure to reach the application before
// anything of the caller's request was sent. A caller that gave up is told so
// with its own context error.
func (a *Adapter) unavailable(ctx context.Context, err error, msg string) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), msg).WithDetails("transport", Transport)
}

// listFailure is the error of a failed tool listing, which also drops the
// session unless the server answered with a proper error.
func (a *Adapter) listFailure(ctx context.Context, s *session, err error, tripsBefore int64) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	switch {
	case errors.Is(err, errTooManyTools):
		return typed(contracts.ErrSchemaInvalid, "the application lists an unreasonable number of tools", ReasonTooManyTools)
	case errors.Is(err, errTooLarge) || s.ep.trips.Load() != tripsBefore:
		s.ep.discard(s)
		return responseTooLarge(a.cfg.MaxResponseBytes)
	}
	if !isRPC(err) {
		s.ep.discard(s)
	}
	return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application's tools could not be listed").WithDetails("transport", Transport)
}

func isRPC(err error) bool {
	var je *jsonrpc.Error
	return errors.As(err, &je)
}

// call sends one tool call and returns the structured part of its result. It
// never retries: the one exception is a session the server says it does not
// know (the request was refused, not processed), which is opened again once.
func (a *Adapter) call(ctx context.Context, rt *route, args map[string]any, commit bool) (reply, error) {
	if err := ctx.Err(); err != nil {
		return reply{}, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return reply{}, contracts.New(contracts.ErrValidationFailed, "adapter request: the arguments cannot be encoded as JSON")
	}
	cctx := ctx
	if rt.timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, rt.timeout)
		defer cancel()
	}
	for attempt := 0; ; attempt++ {
		s, err := rt.ep.acquire(cctx)
		if err != nil {
			return reply{}, a.stillUnavailable(ctx, cctx, err)
		}
		rep, err, resend := a.callOn(ctx, cctx, s, rt, raw, commit)
		s.release()
		if resend && attempt == 0 {
			continue
		}
		return rep, err
	}
}

// stillUnavailable keeps the caller's context error for a caller that gave up,
// and tells a call-level timeout from the caller's own.
func (a *Adapter) stillUnavailable(ctx, cctx context.Context, err error) error {
	if ctx.Err() == nil && cctx.Err() != nil && errors.Is(err, cctx.Err()) {
		return fmt.Errorf("the tool call timed out: %w", cctx.Err())
	}
	return err
}

func (a *Adapter) callOn(ctx, cctx context.Context, s *session, rt *route, args json.RawMessage, commit bool) (reply, error, bool) {
	ep := rt.ep
	if err := ep.ensureSurface(cctx, s); err != nil {
		return reply{}, a.stillUnavailable(ctx, cctx, err), false
	}
	switch st, d := ep.state(rt.tool); st {
	case toolDrifted:
		return reply{}, quarantinedErr(d), false
	case toolAbsent:
		return reply{}, notOfferedErr(rt.tool), false
	}
	tripsBefore := ep.trips.Load()
	bound, done := s.bind(cctx)
	defer done()
	res, err := s.cs.CallTool(bound, &mcp.CallToolParams{Name: rt.tool, Arguments: args})
	if err != nil {
		return reply{}, a.callFailure(ctx, cctx, bound, s, err, tripsBefore, commit), errors.Is(err, mcp.ErrSessionMissing) && !redirected(bound)
	}
	rep, err := a.replyOf(res, commit)
	return rep, err, false
}

// callFailure types a failed CallTool. Where the request provably was not
// delivered the answer is unavailable, whatever the operation; where it may have
// been, a Commit is uncertain_outcome (the caller asks Status before anything
// else) and every other operation is unavailable. A request that was answered
// with a redirect was delivered, whatever the later hops say.
func (a *Adapter) callFailure(ctx, cctx, bound context.Context, s *session, err error, tripsBefore int64, commit bool) error {
	ep := s.ep
	hopped := redirected(bound)
	switch {
	case ctx.Err() == nil && errors.Is(context.Cause(bound), errSessionClosed):
		// The session was closed under the call: the adapter is gone or was
		// superseded. The request may have been delivered.
		if commit {
			return adapters.UncertainOutcome(errors.New("the connection was closed"))
		}
		return contracts.Wrap(contracts.ErrUnavailable, errors.New("the connection was closed"), "the application did not complete the call").WithDetails("transport", Transport)
	case errors.Is(err, errTooLarge) || ep.trips.Load() != tripsBefore:
		ep.discard(s)
		return responseTooLarge(a.cfg.MaxResponseBytes)
	case ctx.Err() != nil:
		return ctx.Err() // the caller's cancellation or deadline; adapters.Wrap types it
	case cctx.Err() != nil:
		return fmt.Errorf("the tool call timed out: %w", cctx.Err())
	case !hopped && notSent(err):
		if !errors.Is(err, mcp.ErrSessionMissing) {
			// The connection failed; a session over it is of no use.
			ep.discard(s)
		}
		return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application could not be reached").WithDetails("transport", Transport)
	case !hopped && errors.Is(err, mcp.ErrSessionMissing):
		ep.discard(s)
		return contracts.Wrap(contracts.ErrUnavailable, errors.New("the server no longer knows the session"), "the application dropped the session").WithDetails("transport", Transport)
	}
	if !isRPC(err) {
		ep.discard(s)
	}
	if commit {
		return adapters.UncertainOutcome(safeCause(err))
	}
	return contracts.Wrap(contracts.ErrUnavailable, safeCause(err), "the application did not complete the call").WithDetails("transport", Transport)
}

// replyOf takes the structured content of a tool result. Text is never read: a
// result is a fact only when the server states it as structured content, and
// text (even text that parses as a result, which the clinic application mirrors
// beside its structured content) is for people.
func (a *Adapter) replyOf(res *mcp.CallToolResult, commit bool) (reply, error) {
	switch {
	case res == nil:
		return reply{}, typed(contracts.ErrSchemaInvalid, "the application answered with no result", "")
	case res.IsError:
		// A refusal is a result with status rejected, never a tool error: an
		// error means the call failed in a way the application could not state.
		if commit {
			return reply{}, adapters.UncertainOutcome(errors.New("the tool reported an error"))
		}
		return reply{}, typed(contracts.ErrUnavailable, "the tool reported an error", ReasonToolError)
	case res.NeedsInput():
		return reply{}, typed(contracts.ErrSchemaInvalid, "the application asked for input the adapter cannot give", ReasonInputRequired)
	case res.StructuredContent == nil:
		return reply{}, typed(contracts.ErrSchemaInvalid, "the tool answered with text and no structured content", ReasonTextOnlyResult)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return reply{}, typed(contracts.ErrSchemaInvalid, "the structured content cannot be read", "")
	}
	if int64(len(b)) > a.cfg.MaxResponseBytes {
		return reply{}, responseTooLarge(a.cfg.MaxResponseBytes)
	}
	if len(b) == 0 || b[0] != '{' {
		return reply{}, typed(contracts.ErrSchemaInvalid, "the structured content is not a JSON object", ReasonStructuredNotObject)
	}
	rep := reply{structured: b}
	if v, ok := res.Meta[resultMetaKey].(string); ok {
		rep.echoedKey = v
	}
	return rep, nil
}
