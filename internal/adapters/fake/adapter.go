package fake

import (
	"bytes"
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Transport is the transport name the fake adapter reports in Describe.
const Transport = "fake"

// Adapter is an adapters.AppAdapter over an App with no wire. It does what a
// real transport must do and nothing more: send only adapters.Identity, read
// replies through adapters.ReadLimited, normalise them with adapters.Normalise,
// never retry, honour the context and classify failures by the rules on
// adapters.AppAdapter. It does not validate scopes or requests and does not
// bound time or concurrency: that is adapters.Wrap, and production wiring (and
// the conformance suite) wraps it.
type Adapter struct {
	app   *App
	limit int64
}

// AdapterOption configures NewAdapter.
type AdapterOption func(*Adapter)

// WithMaxResponseBytes sets the reply size limit; the default is
// adapters.DefaultMaxResponseBytes.
func WithMaxResponseBytes(n int64) AdapterOption { return func(a *Adapter) { a.limit = n } }

// NewAdapter returns an adapter over app.
func NewAdapter(app *App, opts ...AdapterOption) *Adapter {
	a := &Adapter{app: app, limit: adapters.DefaultMaxResponseBytes}
	for _, o := range opts {
		o(a)
	}
	return a
}

var _ adapters.AppAdapter = (*Adapter)(nil)

// App returns the host behind the adapter.
func (a *Adapter) App() *App { return a.app }

// send delivers a request to the host and returns the reply body, read through
// the size limit, plus the echoed key.
func (a *Adapter) send(ctx context.Context, req Request) (body []byte, echoed string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err // nothing was sent
	}
	reply := a.app.Handle(ctx, req)
	switch reply.Action {
	case Refuse:
		return nil, "", contracts.New(contracts.ErrUnavailable, "the application is unreachable")
	case Hang:
		<-ctx.Done()
		return nil, "", ctx.Err()
	case Drop:
		lost := errors.New("the connection was lost before a reply arrived")
		if req.Operation == adapters.OpCommit {
			return nil, "", adapters.UncertainOutcome(lost) // the request may have been processed
		}
		return nil, "", contracts.Wrap(contracts.ErrUnavailable, lost, "the application did not answer")
	}
	raw, err := adapters.ReadLimited(bytes.NewReader(reply.Body), a.limit)
	if err != nil {
		return nil, "", err
	}
	return raw, reply.EchoedKey, nil
}

func (a *Adapter) result(ctx context.Context, scope contracts.Scope, op adapters.Operation, req Request) (actions.DeterministicResult, error) {
	req.Operation = op
	req.Identity = adapters.IdentityFrom(scope)
	raw, echoed, err := a.send(ctx, req)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return adapters.Normalise(raw, adapters.Expect{
		Operation:            op,
		CapabilityID:         req.CapabilityID,
		IdempotencyKey:       req.IdempotencyKey,
		EchoedIdempotencyKey: echoed,
		MaxBytes:             int(a.limit),
	})
}

func (a *Adapter) Describe(ctx context.Context) (adapters.Description, error) {
	raw, _, err := a.send(ctx, Request{Operation: adapters.OpDescribe})
	if err != nil {
		return adapters.Description{}, err
	}
	return adapters.ParseDescription(Transport, raw)
}

func (a *Adapter) Read(ctx context.Context, scope contracts.Scope, r adapters.ReadRequest) (actions.DeterministicResult, error) {
	return a.result(ctx, scope, adapters.OpRead, Request{CapabilityID: r.CapabilityID, Fields: r.Fields})
}

func (a *Adapter) Validate(ctx context.Context, scope contracts.Scope, r adapters.ValidateRequest) (actions.DeterministicResult, error) {
	return a.result(ctx, scope, adapters.OpValidate, Request{CapabilityID: r.CapabilityID, Fields: r.Fields})
}

func (a *Adapter) Prepare(ctx context.Context, scope contracts.Scope, r adapters.PrepareRequest) (actions.DeterministicResult, error) {
	return a.result(ctx, scope, adapters.OpPrepare, Request{CapabilityID: r.CapabilityID, Fields: r.Fields})
}

func (a *Adapter) Commit(ctx context.Context, scope contracts.Scope, r adapters.CommitRequest) (actions.DeterministicResult, error) {
	p := r.Prepared
	return a.result(ctx, scope, adapters.OpCommit, Request{CapabilityID: r.CapabilityID, IdempotencyKey: string(r.IdempotencyKey), Prepared: &p})
}

func (a *Adapter) Status(ctx context.Context, scope contracts.Scope, r adapters.StatusRequest) (actions.DeterministicResult, error) {
	return a.result(ctx, scope, adapters.OpStatus, Request{CapabilityID: r.CapabilityID, IdempotencyKey: string(r.IdempotencyKey)})
}

func (a *Adapter) Cancel(ctx context.Context, scope contracts.Scope, r adapters.CancelRequest) (actions.DeterministicResult, error) {
	return a.result(ctx, scope, adapters.OpCancel, Request{CapabilityID: r.CapabilityID, IdempotencyKey: string(r.IdempotencyKey)})
}

func (a *Adapter) Health(ctx context.Context) (adapters.HealthReport, error) {
	raw, _, err := a.send(ctx, Request{Operation: adapters.OpHealth})
	if err != nil {
		return adapters.HealthReport{}, err
	}
	return adapters.ParseHealth(raw)
}

func (a *Adapter) Inspect(ctx context.Context, scope contracts.Scope, r adapters.InspectRequest) (adapters.Inspection, error) {
	raw, _, err := a.send(ctx, Request{Operation: adapters.OpInspect, Identity: adapters.IdentityFrom(scope), Aspects: r.Aspects})
	if err != nil {
		return adapters.Inspection{}, err
	}
	return adapters.NormaliseInspection(raw, r, int(a.limit))
}
