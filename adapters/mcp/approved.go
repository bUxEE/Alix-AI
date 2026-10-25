package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons the approved-tool call puts in Details["reason"].
const (
	// ReasonToolNotApproved: the tool is not on the endpoint's tools.allow, or the
	// endpoint has no list. Nothing is approved by default.
	ReasonToolNotApproved = "tool_not_approved"
	// ReasonToolMapped: the tool is one a capability maps, or one of the adapter's
	// management tools. Those are reached through the adapter contract
	// (Read, Validate, Prepare, Commit, Status, Describe, Health, Inspect), which
	// checks the scope, the arguments and the result; a call that skipped it would
	// be a way around all three.
	ReasonToolMapped = "tool_is_mapped"
)

// ApprovedReply is what an approved tool answered.
type ApprovedReply struct {
	// Structured is the structured content of the result: one JSON object of at most
	// Config.MaxResponseBytes, as the server sent it. Text is never read.
	Structured json.RawMessage
	// Sent is true once the request may have reached the server, and it is set when
	// the call failed as well: an error with Sent true can be an operation the
	// application applied and whose reply was lost, so a caller that created
	// something must treat what it asked for as possibly existing. It is false for
	// every refusal before the send (not approved, drift, not offered, not
	// reachable).
	Sent bool
}

// ApprovedTool reports whether the adapter configuration of the endpoint approves
// the tool for a call through CallApprovedTool: it is on the endpoint's tools.allow
// and no capability maps it, and it is not a management tool. It asks the server
// nothing.
func (a *Adapter) ApprovedTool(endpointID, tool string) bool {
	_, err := a.approvedEndpoint(endpointID, tool)
	return err == nil
}

// ApprovedToolsOffered reports, for each of the tools, whether the server lists it
// now. The listing is the server's answer of this moment, held to the pins as it is
// before every call, so a server that changed since it was approved is refused as
// drift (quarantined) rather than reported as offering a tool, and a tool seen for
// the first time is pinned. A tool that is not approved is an error
// (permission_denied), not "not offered", so a caller cannot take a refusal for an
// absence.
func (a *Adapter) ApprovedToolsOffered(ctx context.Context, endpointID string, tools ...string) (map[string]bool, error) {
	var e *endpointConn
	for _, t := range tools {
		var err error
		if e, err = a.approvedEndpoint(endpointID, t); err != nil {
			return nil, err
		}
	}
	if e == nil {
		return map[string]bool{}, nil
	}
	s, err := a.listed(ctx, e)
	if err != nil {
		return nil, err
	}
	s.release()
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		switch err := a.checkApproved(e, t); {
		case err == nil:
			out[t] = true
		case notOffered(err):
			out[t] = false
		default:
			return nil, err
		}
	}
	return out, nil
}

// CallApprovedTool calls a tool the adapter configuration approves (tools.allow)
// and no capability maps: the integration audit's test tools (provisioning, change
// of a record, fault injection) are the case. No capability can reach such a tool,
// so no model can; this call is for the platform's own code, and it is held to what
// every call of the adapter is held to:
//
//   - Approval. The tool must be on tools.allow of the endpoint. A tool that is not,
//     and a tool a capability maps or the adapter manages, is refused
//     (permission_denied, ReasonToolNotApproved and ReasonToolMapped) before
//     anything is sent.
//   - Egress. The connection is the adapter's own, made through its Config.Dialer.
//   - Pins and drift. The server's tools are listed again before every call, which
//     checks the mapped tools against their pins and the manifest's contract_sha256.
//     The tool itself is pinned when it is first seen, or against the pin the
//     adapter was given, and a tool that changed, that is listed twice with
//     different definitions, or that disappeared is refused as quarantined
//     (ReasonToolSchemaDrift) and raises the re-audit flag. An adapter that has
//     found drift calls nothing, approved tools included.
//   - Bounds. A reply is cut off at the adapter's response limit, nothing is
//     retried once the request may have been delivered, text is never read, and
//     every error is typed and carries no text the server chose.
//
// args is the tool's argument object as it is (the arguments of these tools are
// their own, not the capability shape {identity, fields}); nil sends none. The
// reply is whatever object the tool answers with: it is not a DeterministicResult
// and is not normalised as one. An isError result is unavailable (ReasonToolError),
// a reply that is text only is schema_invalid (ReasonTextOnlyResult), and a call
// that failed after the request may have been delivered is uncertain_outcome: the
// caller cannot tell whether the application applied it.
func (a *Adapter) CallApprovedTool(ctx context.Context, endpointID, tool string, args map[string]any) (ApprovedReply, error) {
	e, err := a.approvedEndpoint(endpointID, tool)
	if err != nil {
		return ApprovedReply{}, err
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ApprovedReply{}, contracts.New(contracts.ErrValidationFailed, "adapter request: the arguments cannot be encoded as JSON")
	}
	s, err := a.approvedSurface(ctx, e, tool)
	if err != nil {
		return ApprovedReply{}, err
	}
	defer s.release()

	tripsBefore := e.trips.Load()
	bound, done := s.bind(ctx)
	defer done()
	res, err := s.cs.CallTool(bound, &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(raw)})
	if err != nil {
		err = a.callFailure(ctx, ctx, bound, s, err, tripsBefore, true)
		var ce *contracts.Error
		// An unavailable error is the one answer that proves the request never
		// reached a server that could act on it.
		sent := !(errors.As(err, &ce) && ce != nil && ce.Code == contracts.ErrUnavailable)
		return ApprovedReply{Sent: sent}, err
	}
	rep, err := a.replyOf(res, false)
	if err != nil {
		return ApprovedReply{Sent: true}, err
	}
	return ApprovedReply{Structured: rep.structured, Sent: true}, nil
}

// approvedEndpoint checks what needs no network: the endpoint exists, the tool name
// is a tool name, the tool is approved and no capability maps it.
func (a *Adapter) approvedEndpoint(endpointID, tool string) (*endpointConn, error) {
	e := a.byID[endpointID]
	switch {
	case e == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "adapter request: the endpoint is not one of the adapter's").WithDetails("transport", Transport)
	case !toolNameRE.MatchString(tool):
		return nil, contracts.New(contracts.ErrValidationFailed, "adapter request: not a tool name").WithDetails("transport", Transport)
	case e.mapped[tool] || e.managed[tool]:
		return nil, typed(contracts.ErrPermissionDenied, "the tool is reached through the adapter contract, not through an approved-tool call", ReasonToolMapped).
			WithDetails("tool", tool)
	case !e.approved[tool]:
		// approved is nil when the host imposes no list; that approves nothing here.
		return nil, typed(contracts.ErrPermissionDenied, "the adapter configuration does not approve this tool", ReasonToolNotApproved).
			WithDetails("tool", tool)
	}
	return e, nil
}

// approvedSurface returns a session in use over a fresh listing of the endpoint's
// tools that has been held to the pins and in which the tool is offered and
// unchanged. The caller releases the session.
func (a *Adapter) approvedSurface(ctx context.Context, e *endpointConn, tool string) (*session, error) {
	s, err := a.listed(ctx, e)
	if err != nil {
		return nil, err
	}
	if err := a.checkApproved(e, tool); err != nil {
		s.release()
		return nil, err
	}
	return s, nil
}

// listed returns a session in use over a fresh listing of the endpoint's tools,
// held to the pins of the tools a capability maps and to contract_sha256, and
// refuses once the adapter has found drift. A request of the caller's has not been
// sent when it fails, so a session that broke is opened again once. The caller
// releases the session.
func (a *Adapter) listed(ctx context.Context, e *endpointConn) (*session, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.ReauditRequired() {
			return nil, a.driftRefusal(e, "")
		}
		s, err := e.acquire(ctx)
		if err != nil {
			return nil, err
		}
		if err := e.refresh(ctx, s, true); err != nil {
			s.release()
			var ce *contracts.Error
			if attempt == 0 && errors.As(err, &ce) && ce != nil && ce.Code == contracts.ErrUnavailable && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		if a.ReauditRequired() { // the listing just made found a drift
			s.release()
			return nil, a.driftRefusal(e, "")
		}
		return s, nil
	}
}

// driftRefusal is the quarantined error of an adapter that has found drift: the
// tool's own drift when it has one, the endpoint's otherwise.
func (a *Adapter) driftRefusal(e *endpointConn, tool string) error {
	e.smu.Lock()
	d, ok := e.drifts[tool]
	e.smu.Unlock()
	if !ok {
		d = Drift{Endpoint: e.cfg.ID}
		for _, known := range a.Drifts() {
			if known.Endpoint == e.cfg.ID {
				d = known
				break
			}
		}
	}
	return quarantinedErr(d)
}

// checkApproved holds the tool of the last listing to its pin. The refresh checks
// only the tools a capability maps and the management tools, so the pin of an
// approved tool is kept here: taken when the tool is first listed, compared with
// every listing after that. A change is sticky drift, as it is for a mapped tool.
func (a *Adapter) checkApproved(e *endpointConn, tool string) error {
	e.smu.Lock()
	lt, present := e.live.tools[tool]
	pin, pinned := e.pins[tool]
	ambiguous := e.live.ambiguous[tool]
	prior, drifted := e.drifts[tool]
	var found *Drift
	switch {
	case drifted:
		e.smu.Unlock()
		return quarantinedErr(prior)
	case ambiguous:
		found = &Drift{Tool: tool, Kind: DriftAmbiguousName, Pinned: pin}
	case !present && pinned:
		found = &Drift{Tool: tool, Kind: DriftToolMissing, Pinned: pin}
	case !present:
		e.smu.Unlock()
		return notOfferedErr(tool)
	case !pinned:
		e.pins[tool] = lt.hash
	case pin != lt.hash:
		found = &Drift{Tool: tool, Kind: DriftSchemaChanged, Pinned: pin, Observed: lt.hash}
	}
	if found != nil {
		found.Endpoint, found.At = e.cfg.ID, a.cfg.Clock.Now()
		e.drifts[tool] = *found
	}
	e.smu.Unlock()
	if found == nil {
		return nil
	}
	a.raiseDrift(*found)
	return quarantinedErr(*found)
}
