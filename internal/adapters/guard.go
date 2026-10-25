package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Defaults of Options.
const (
	// DefaultCallTimeout bounds a call whose context has no deadline.
	DefaultCallTimeout = 30 * time.Second
	// MaxCallTimeout is the longest any call may run, whatever deadline the
	// caller's context carries. It is the manifest's largest timeout_ms.
	MaxCallTimeout = 120 * time.Second
	// DefaultMaxInFlight is how many calls one adapter runs at once.
	DefaultMaxInFlight = 32
)

// Options configures Wrap.
type Options struct {
	// ApplicationID is the application this adapter is bound to. Required. A
	// request whose scope names another application is refused.
	ApplicationID string
	// DefaultTimeout bounds a call whose context carries no deadline. Zero means
	// DefaultCallTimeout.
	DefaultTimeout time.Duration
	// MaxTimeout caps every call, including calls with a longer deadline. Zero
	// means MaxCallTimeout.
	MaxTimeout time.Duration
	// MaxInFlight is how many calls may run at once; one more is refused with
	// resource_exhausted instead of queueing behind a hung application. Zero
	// means DefaultMaxInFlight. Health is not counted, so it can still report
	// when every slot is stuck.
	MaxInFlight int
}

// Wrap puts the uniform rules around a transport. The rest of the core calls
// the wrapper, never a transport directly, so these rules hold whichever
// transport is behind it (master plan 9.3: transports are not the policy layer;
// they are not allowed to weaken it either).
//
// Before the transport is called, in this order, and with no I/O:
//
//  1. the scope must be valid (every level present, no wildcards) and bound to
//     Options.ApplicationID: scope_missing or scope_violation otherwise;
//  2. the request must be well formed (identifiers, fields, idempotency key,
//     and for Commit the prepared action): validation_failed otherwise;
//  3. the capability must be one the context token permits: scope_violation
//     otherwise;
//  4. a free in-flight slot must exist: resource_exhausted otherwise.
//
// The call then runs under a deadline: the caller's if it has one, otherwise
// DefaultTimeout, never longer than MaxTimeout. After it returns:
//
//   - every result is re-validated with CheckResult against the call that was
//     made, so a transport that skips Normalise, or a bug in it, still cannot
//     deliver a result that is not a DeterministicResult for this operation and
//     capability;
//   - an untyped error becomes a typed one (timeout, internal) so no failure is
//     a bare string; cancellation by the caller stays an error wrapping
//     context.Canceled;
//   - a Commit that fails after it may have reached the host (timeout, an
//     unusable reply, an internal fault, cancellation) is reported as
//     uncertain_outcome with the cause attached, because the effect may have
//     been applied: the caller must ask Status before it does anything else. A
//     Commit failure that proves nothing was applied (unavailable, a scope or
//     validation error, a host refusal) keeps its own code.
//
// The wrapper never retries anything, and a panic in the transport becomes an
// internal error (uncertain_outcome for Commit) instead of crashing the service.
func Wrap(inner AppAdapter, opts Options) (AppAdapter, error) {
	if inner == nil {
		return nil, contracts.New(contracts.ErrInternal, "adapters: Wrap needs an adapter")
	}
	if !validIdentifier(opts.ApplicationID) {
		return nil, contracts.New(contracts.ErrValidationFailed, "adapters: Options.ApplicationID is empty or malformed")
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = DefaultCallTimeout
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = MaxCallTimeout
	}
	if opts.DefaultTimeout > opts.MaxTimeout {
		opts.DefaultTimeout = opts.MaxTimeout
	}
	if opts.MaxInFlight <= 0 {
		opts.MaxInFlight = DefaultMaxInFlight
	}
	return &guard{inner: inner, opts: opts, slots: make(chan struct{}, opts.MaxInFlight)}, nil
}

type guard struct {
	inner AppAdapter
	opts  Options
	slots chan struct{}
}

// admit is steps 1 to 3: scope, request, capability.
func (g *guard) admit(scope contracts.Scope, validate func() error, capabilityID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if scope.ApplicationID != g.opts.ApplicationID {
		return contracts.New(contracts.ErrScopeViolation, "the scope is bound to another application")
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	if capabilityID != "" && !scope.Permits(capabilityID) {
		return contracts.New(contracts.ErrScopeViolation, "the context token does not permit this capability")
	}
	return nil
}

// acquire takes an in-flight slot without waiting.
func (g *guard) acquire() (release func(), err error) {
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, nil
	default:
		return nil, contracts.New(contracts.ErrResourceExhausted, "the adapter has too many calls in flight")
	}
}

// callContext derives the context a transport call runs under.
func (g *guard) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithTimeout(ctx, g.opts.MaxTimeout)
	}
	return context.WithTimeout(ctx, g.opts.DefaultTimeout)
}

// run executes call under the deadline, with panics contained.
func run[T any](ctx context.Context, call func(context.Context) (T, error)) (out T, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			out, err = zero, contracts.New(contracts.ErrInternal, "adapter panicked")
		}
	}()
	return call(ctx)
}

// mapError gives a transport failure a typed form.
func mapError(parent, call context.Context, op Operation, err error) error {
	var typed *contracts.Error
	switch {
	case errors.As(err, &typed):
		return err
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(call.Err(), context.DeadlineExceeded):
		return contracts.Wrap(contracts.ErrTimeout, err, "the application did not answer in time")
	case errors.Is(err, context.Canceled) || parent.Err() != nil:
		return fmt.Errorf("adapters: %s canceled: %w", op, context.Canceled)
	}
	return contracts.Wrap(contracts.ErrInternal, err, "the adapter failed")
}

// commitOutcome applies the uncertain-outcome rule to a failed Commit.
func commitOutcome(err error) error {
	if definiteCodes[contracts.CodeOf(err)] {
		return err
	}
	return UncertainOutcome(err)
}

// result is the shared path of the six operations that return a
// DeterministicResult.
func (g *guard) result(
	ctx context.Context,
	op Operation,
	scope contracts.Scope,
	capabilityID, key string,
	validate func() error,
	call func(context.Context) (actions.DeterministicResult, error),
) (actions.DeterministicResult, error) {
	var zero actions.DeterministicResult
	if err := g.admit(scope, validate, capabilityID); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		// Done before anything was sent: nothing can have been applied, so this
		// is a plain timeout or cancellation even for Commit.
		return zero, mapError(ctx, ctx, op, err)
	}
	release, err := g.acquire()
	if err != nil {
		return zero, err
	}
	defer release()
	cctx, cancel := g.callContext(ctx)
	defer cancel()

	res, err := run(cctx, call)
	if err == nil {
		res, err = CheckResult(res, Expect{Operation: op, CapabilityID: capabilityID, IdempotencyKey: key})
	}
	if err != nil {
		err = mapError(ctx, cctx, op, err)
		if op == OpCommit {
			err = commitOutcome(err)
		}
		return zero, err
	}
	return res, nil
}

func (g *guard) Describe(ctx context.Context) (Description, error) {
	release, err := g.acquire()
	if err != nil {
		return Description{}, err
	}
	defer release()
	cctx, cancel := g.callContext(ctx)
	defer cancel()
	d, err := run(cctx, g.inner.Describe)
	if err == nil {
		err = d.Validate()
	}
	if err != nil {
		return Description{}, mapError(ctx, cctx, OpDescribe, err)
	}
	return d, nil
}

func (g *guard) Read(ctx context.Context, scope contracts.Scope, req ReadRequest) (actions.DeterministicResult, error) {
	return g.result(ctx, OpRead, scope, req.CapabilityID, "", req.Validate,
		func(c context.Context) (actions.DeterministicResult, error) { return g.inner.Read(c, scope, req) })
}

func (g *guard) Validate(ctx context.Context, scope contracts.Scope, req ValidateRequest) (actions.DeterministicResult, error) {
	return g.result(ctx, OpValidate, scope, req.CapabilityID, "", req.Validate,
		func(c context.Context) (actions.DeterministicResult, error) { return g.inner.Validate(c, scope, req) })
}

func (g *guard) Prepare(ctx context.Context, scope contracts.Scope, req PrepareRequest) (actions.DeterministicResult, error) {
	return g.result(ctx, OpPrepare, scope, req.CapabilityID, "", req.Validate,
		func(c context.Context) (actions.DeterministicResult, error) { return g.inner.Prepare(c, scope, req) })
}

func (g *guard) Commit(ctx context.Context, scope contracts.Scope, req CommitRequest) (actions.DeterministicResult, error) {
	return g.result(ctx, OpCommit, scope, req.CapabilityID, string(req.IdempotencyKey), req.Validate,
		func(c context.Context) (actions.DeterministicResult, error) { return g.inner.Commit(c, scope, req) })
}

func (g *guard) Status(ctx context.Context, scope contracts.Scope, req StatusRequest) (actions.DeterministicResult, error) {
	return g.result(ctx, OpStatus, scope, req.CapabilityID, string(req.IdempotencyKey), req.Validate,
		func(c context.Context) (actions.DeterministicResult, error) { return g.inner.Status(c, scope, req) })
}

func (g *guard) Cancel(ctx context.Context, scope contracts.Scope, req CancelRequest) (actions.DeterministicResult, error) {
	return g.result(ctx, OpCancel, scope, req.CapabilityID, string(req.IdempotencyKey), req.Validate,
		func(c context.Context) (actions.DeterministicResult, error) { return g.inner.Cancel(c, scope, req) })
}

// Health reports the application's state. A probe that fails is a report with
// status unavailable and a fixed reason, never the adapter's error text and
// never an error, so a caller has one shape to handle; only the caller's own
// cancellation or deadline is returned as an error.
func (g *guard) Health(ctx context.Context) (HealthReport, error) {
	cctx, cancel := g.callContext(ctx)
	defer cancel()
	h, err := run(cctx, g.inner.Health)
	if err == nil {
		err = h.Validate()
	}
	if err != nil {
		if perr := ctx.Err(); perr != nil {
			return HealthReport{}, mapError(ctx, cctx, OpHealth, perr)
		}
		return HealthReport{Status: HealthUnavailable, Reason: healthReason(cctx, err)}, nil
	}
	return h, nil
}

// healthReason is the fixed explanation for a failed probe.
func healthReason(call context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(call.Err(), context.DeadlineExceeded):
		return "health probe timed out"
	}
	switch contracts.CodeOf(err) {
	case contracts.ErrUnavailable:
		return "application unreachable"
	case contracts.ErrTimeout:
		return "health probe timed out"
	case contracts.ErrUnauthenticated, contracts.ErrPermissionDenied:
		return "application refused the adapter's credentials"
	case contracts.ErrSchemaInvalid:
		return "application returned an invalid health report"
	}
	return "health probe failed"
}

func (g *guard) Inspect(ctx context.Context, scope contracts.Scope, req InspectRequest) (Inspection, error) {
	if err := g.admit(scope, req.Validate, ""); err != nil {
		return Inspection{}, err
	}
	release, err := g.acquire()
	if err != nil {
		return Inspection{}, err
	}
	defer release()
	cctx, cancel := g.callContext(ctx)
	defer cancel()
	in, err := run(cctx, func(c context.Context) (Inspection, error) { return g.inner.Inspect(c, scope, req) })
	if err == nil {
		in, err = CheckInspection(in, req)
	}
	if err != nil {
		return Inspection{}, mapError(ctx, cctx, OpInspect, err)
	}
	return in, nil
}
