package native

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

type callOpts struct {
	// commit says that a failure after the request was sent must be reported as
	// uncertain_outcome.
	commit  bool
	timeout time.Duration
}

// reply is the usable part of a response.
type reply struct {
	result    []byte
	echoedKey string
}

// dial opens the connection to a bridge: a Unix socket whose permissions have
// been checked, or loopback TCP through the egress dialer.
func (a *Adapter) dial(ctx context.Context, ep *endpoint) (net.Conn, error) {
	switch ep.cfg.Network {
	case NetworkUnix:
		if err := checkSocket(ep.cfg.Address); err != nil {
			a.cfg.Logger.Warn("a native bridge socket was refused",
				slog.String("application_id", a.cfg.ApplicationID), slog.String("endpoint", ep.cfg.ID), slog.String("reason", err.Error()))
			return nil, refusal(ReasonSocketPermissions, "the bridge socket is not restricted to its owner: "+err.Error())
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", ep.cfg.Address)
	default:
		return a.cfg.Dialer.DialContext(ctx, "tcp", ep.cfg.Address)
	}
}

// exchange opens one connection, authenticates both ends, sends one request and
// reads its reply. It never retries, and a connection serves one call. Before the
// request is written nothing of the call has left the process; the failures up to
// then are unavailable, a Commit's included.
func (a *Adapter) exchange(ctx context.Context, ep *endpoint, method string, params any, o callOpts) (reply, error) {
	if err := ctx.Err(); err != nil {
		return reply{}, err // nothing was sent
	}
	if a.closeCtx.Err() != nil {
		return reply{}, refusal(ReasonAdapterClosed, "the adapter was closed")
	}
	p, err := json.Marshal(params)
	if err != nil {
		return reply{}, contracts.New(contracts.ErrValidationFailed, "adapter request: the arguments cannot be encoded as JSON")
	}
	req, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: idCall, Method: method, Params: p})
	if err != nil {
		return reply{}, contracts.New(contracts.ErrValidationFailed, "adapter request: the request cannot be encoded as JSON")
	}
	if int64(len(req)) > a.cfg.MaxRequestBytes {
		return reply{}, typed(contracts.ErrValidationFailed, "adapter request: the request is larger than the frame limit", ReasonRequestTooLarge).
			WithDetails("limit_bytes", a.cfg.MaxRequestBytes)
	}

	// The call runs under the caller's context, the binding's timeout and the
	// adapter's own lifetime: Close ends a call in flight, and the end of any of
	// them closes the connection, which unblocks a read.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopLife := context.AfterFunc(a.closeCtx, cancel)
	defer stopLife()
	if o.timeout > 0 {
		var cancelT context.CancelFunc
		cctx, cancelT = context.WithTimeout(cctx, o.timeout)
		defer cancelT()
	}
	sent := false
	fail := func(err error) error { return a.failure(ctx, cctx, err, sent, o.commit) }

	conn, err := a.dial(cctx, ep)
	if err != nil {
		return reply{}, fail(err)
	}
	defer conn.Close()
	stopConn := context.AfterFunc(cctx, func() { _ = conn.Close() })
	defer stopConn()

	if ep.cfg.PeerCheck != nil {
		if cr, ok := peerCreds(conn); ok {
			if err := ep.cfg.PeerCheck(cr); err != nil {
				a.cfg.Logger.Warn("the operating system's account of a native bridge was refused",
					slog.String("application_id", a.cfg.ApplicationID), slog.String("endpoint", ep.cfg.ID), slog.Int("peer_pid", cr.PID))
				return reply{}, fail(refusal(ReasonPeerRefused, "the bridge process is not the one the operator allowed"))
			}
		}
	}

	_ = conn.SetDeadline(time.Now().Add(a.cfg.HandshakeTimeout))
	ks, err := a.handshake(conn, ep)
	if err != nil {
		if errors.Is(err, errPeerAuth) {
			a.cfg.Logger.Warn("a native bridge did not authenticate",
				slog.String("application_id", a.cfg.ApplicationID), slog.String("endpoint", ep.cfg.ID))
		}
		return reply{}, fail(err)
	}
	_ = conn.SetDeadline(time.Time{})

	sent = true // from here the host may have received the request
	if err := writeSealed(conn, ks, labelRequest, req, int(a.cfg.MaxRequestBytes)); err != nil {
		return reply{}, fail(err)
	}
	body, err := readSealed(conn, ks, labelReply, int(a.cfg.MaxResponseBytes)+envelopeSlack)
	switch {
	case errors.Is(err, errBadMAC):
		// A reply that is not sealed for this session did not come from the bridge it
		// was authenticated to: a relay altered it, or made it. None of it is used.
		return reply{}, fail(typed(contracts.ErrSchemaInvalid, "the reply is not authentic for this session", ReasonProtocol))
	case errors.Is(err, errFrameTooLarge):
		return reply{}, fail(responseTooLarge(a.cfg.MaxResponseBytes))
	case errors.Is(err, errFrameEmpty):
		return reply{}, fail(typed(contracts.ErrSchemaInvalid, "the reply is an empty frame", ReasonProtocol))
	case err != nil:
		return reply{}, fail(err)
	}
	resp, err := decodeResponse(body, idCall)
	if err != nil {
		return reply{}, fail(typed(contracts.ErrSchemaInvalid, "the reply is not a JSON-RPC response to this call", ReasonProtocol))
	}
	if resp.Error != nil {
		return reply{}, fail(hostError(resp.Error))
	}
	if int64(len(resp.Result)) > a.cfg.MaxResponseBytes {
		return reply{}, fail(responseTooLarge(a.cfg.MaxResponseBytes))
	}
	rep := reply{result: resp.Result}
	if resp.Meta != nil {
		rep.echoedKey = resp.Meta.IdempotencyKey
	}
	return rep, nil
}

// result sends one operation that answers with a DeterministicResult and turns
// the reply into one: the one place a reply becomes a fact.
func (a *Adapter) result(ctx context.Context, op adapters.Operation, capID string, rt *route, params any, key string) (actions.DeterministicResult, error) {
	commit := op == adapters.OpCommit
	rep, err := a.exchange(ctx, rt.ep, rt.method, params, callOpts{commit: commit, timeout: rt.timeout})
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	res, err := adapters.Normalise(rep.result, adapters.Expect{
		Operation:            op,
		CapabilityID:         capID,
		IdempotencyKey:       key,
		EchoedIdempotencyKey: rep.echoedKey,
		MaxBytes:             int(a.cfg.MaxResponseBytes),
	})
	if err != nil {
		return actions.DeterministicResult{}, afterReply(commit, err)
	}
	return res, nil
}
