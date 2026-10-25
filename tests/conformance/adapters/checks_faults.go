package adaptersuite

import (
	"context"
	"errors"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func health(ctx context.Context, s Subject) (adapters.HealthReport, error) {
	r, werr := bounded(ctx, "health", func() (adapters.HealthReport, error) { return s.Adapter.Health(ctx) })
	if werr != nil {
		return adapters.HealthReport{}, werr
	}
	return r.v, r.err
}

func inspect(ctx context.Context, s Subject, sc contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	r, werr := bounded(ctx, "inspect", func() (adapters.Inspection, error) { return s.Adapter.Inspect(ctx, sc, req) })
	if werr != nil {
		return adapters.Inspection{}, werr
	}
	return r.v, r.err
}

// stillWorks proves the adapter is usable after a fault: a plain read gets an
// answer and no error.
func stillWorks(ctx context.Context, s Subject, after string) error {
	o, err := readItem(ctx, s, scopeA(s), "item_1")
	if err != nil {
		return failf("after %s: %v", after, err)
	}
	if o.err != nil {
		return failf("after %s the adapter is wedged: a plain read failed with %v", after, o.err)
	}
	return nil
}

// call is one operation that can be invoked with any context, for the checks
// that run every operation through the same fault.
type call struct {
	op  adapters.Operation
	run func(ctx context.Context) (outcome, error)
}

// resultCalls returns one call per operation that returns a DeterministicResult.
// Commit, status and cancel use p's key; commit also sends p.
func resultCalls(s Subject, sc contracts.Scope, p actions.PreparedAction) []call {
	return []call{
		{adapters.OpRead, func(ctx context.Context) (outcome, error) { return readItem(ctx, s, sc, "item_1") }},
		{adapters.OpValidate, func(ctx context.Context) (outcome, error) {
			return validateName(ctx, s, sc, map[string]any{"name": "Widget"})
		}},
		{adapters.OpPrepare, func(ctx context.Context) (outcome, error) { return prepareName(ctx, s, sc, "Widget") }},
		{adapters.OpCommit, func(ctx context.Context) (outcome, error) { return commit(ctx, s, sc, p) }},
		{adapters.OpStatus, func(ctx context.Context) (outcome, error) { return status(ctx, s, sc, p.IdempotencyKey) }},
		{adapters.OpCancel, func(ctx context.Context) (outcome, error) { return cancelKey(ctx, s, sc, p.IdempotencyKey) }},
	}
}

func checkTimeoutsAndCancellation(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)
	const deadline = 150 * time.Millisecond

	// A host that never answers: every call is cut off at its deadline, with an
	// error that wraps context.DeadlineExceeded. A commit that was sent may
	// have been applied, so it is uncertain_outcome; the others are timeout.
	// A call cut off before it reached the host (a loaded machine) proves nothing
	// either way, so it is tried again a few times; a call that reached the host
	// more than once is a retry, and fails at once.
	const unreachedTries = 3
	ops := []adapters.Operation{adapters.OpRead, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus}
	unreached := map[adapters.Operation]int{}
	for i := 0; i < len(ops); i++ {
		op := ops[i]
		p, err := prepared(ctx, s, sc, "Widget")
		if err != nil {
			return err
		}
		var c call
		for _, cc := range resultCalls(s, sc, p) {
			if cc.op == op {
				c = cc
			}
		}
		fx.Inject(op, fake.FaultHang)
		before, effects := callCount(s), fx.Effects()
		cctx, cancel := context.WithTimeout(ctx, deadline)
		start := time.Now()
		o, err := c.run(cctx)
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			return err
		}
		if o.err == nil {
			return failf("%s to a host that never answers returned %s instead of an error", op, o.res.Status)
		}
		if !errors.Is(o.err, context.DeadlineExceeded) {
			return failf("%s to a host that never answers: %v does not wrap context.DeadlineExceeded", op, o.err)
		}
		want := contracts.ErrTimeout
		if op == adapters.OpCommit {
			want = contracts.ErrUncertainOutcome
		}
		if code(o.err) != want {
			return failf("%s to a host that never answers: code %s, want %s", op, code(o.err), want)
		}
		if !zeroResult(o.res) {
			return failf("%s returned a result alongside its timeout error", op)
		}
		if elapsed > deadline+grace {
			return failf("%s took %s to give up on a %s deadline", op, elapsed, deadline)
		}
		if n := len(newCalls(s, before)); n == 0 && unreached[op] < unreachedTries-1 {
			// The hang it was meant for is still queued: clear it, and let a plain
			// call through so a request that arrives late is counted before the
			// next try, not in it.
			fx.ClearFaults()
			if err := stillWorks(ctx, s, "an unanswered "+string(op)); err != nil {
				return err
			}
			unreached[op]++
			i-- // the same operation again
			continue
		} else if n != 1 {
			return failf("%s reached the host %d times; a timed-out call must not be retried", op, n)
		}
		if fx.Effects() != effects {
			return failf("%s changed the application while it was hung", op)
		}
		if err := stillWorks(ctx, s, "a timed-out "+string(op)); err != nil {
			return err
		}
	}

	// Cancellation by the caller while the host hangs.
	for _, op := range []adapters.Operation{adapters.OpRead, adapters.OpCommit} {
		p, err := prepared(ctx, s, sc, "Widget")
		if err != nil {
			return err
		}
		var c call
		for _, cc := range resultCalls(s, sc, p) {
			if cc.op == op {
				c = cc
			}
		}
		fx.Inject(op, fake.FaultHang)
		cctx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		o, err := c.run(cctx)
		elapsed := time.Since(start)
		timer.Stop()
		cancel()
		if err != nil {
			return err
		}
		if o.err == nil {
			return failf("a cancelled %s returned %s instead of an error", op, o.res.Status)
		}
		if !errors.Is(o.err, context.Canceled) || errors.Is(o.err, context.DeadlineExceeded) {
			return failf("a cancelled %s: %v must wrap context.Canceled and not report a deadline", op, o.err)
		}
		if op == adapters.OpCommit && code(o.err) != contracts.ErrUncertainOutcome {
			return failf("a commit cancelled after it was sent is %s, want uncertain_outcome", code(o.err))
		}
		if op == adapters.OpRead && code(o.err) == contracts.ErrUncertainOutcome {
			return failf("a cancelled read is reported as uncertain_outcome")
		}
		if elapsed > grace {
			return failf("a cancelled %s took %s to return", op, elapsed)
		}
		if err := stillWorks(ctx, s, "a cancelled "+string(op)); err != nil {
			return err
		}
	}

	// A context that is already done sends nothing, and a commit that was never
	// sent is not "uncertain".
	p, err := prepared(ctx, s, sc, "Widget")
	if err != nil {
		return err
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range resultCalls(s, sc, p) {
		before := callCount(s)
		o, err := c.run(done)
		if err != nil {
			return err
		}
		if o.err == nil || !errors.Is(o.err, context.Canceled) {
			return failf("%s with a cancelled context: %v, want an error wrapping context.Canceled", c.op, o.err)
		}
		if n := len(newCalls(s, before)); n != 0 {
			return failf("%s with a cancelled context reached the host", c.op)
		}
		if code(o.err) == contracts.ErrUncertainOutcome {
			return failf("%s with a cancelled context is uncertain_outcome although nothing was sent", c.op)
		}
	}
	return nil
}

func checkMalformedAndPartialResponses(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	faults := []fake.Fault{fake.FaultMalformed, fake.FaultPartial, fake.FaultTextOnly, fake.FaultGeneric, fake.FaultWrongCapability}
	for _, op := range adapters.ResultOperations() {
		fl := faults
		if op == adapters.OpCommit || op == adapters.OpStatus || op == adapters.OpCancel {
			fl = append(append([]fake.Fault(nil), faults...), fake.FaultWrongKey)
		}
		for _, f := range fl {
			p, err := prepared(ctx, s, sc, "Widget")
			if err != nil {
				return err
			}
			var c call
			for _, cc := range resultCalls(s, sc, p) {
				if cc.op == op {
					c = cc
				}
			}
			fx.Inject(op, f)
			before, effects := callCount(s), fx.Effects()
			o, err := c.run(ctx)
			if err != nil {
				return err
			}
			what := string(op) + " with a " + string(f) + " reply"
			if o.err == nil {
				return failf("%s was accepted (status %s): a reply that is not a DeterministicResult for this call must never be used", what, o.res.Status)
			}
			if !contracts.HasCode(o.err, contracts.ErrSchemaInvalid) {
				return failf("%s: %v, want schema_invalid", what, o.err)
			}
			if !zeroResult(o.res) {
				return failf("%s returned a result alongside the error: %+v", what, o.res)
			}
			if op == adapters.OpCommit {
				if code(o.err) != contracts.ErrUncertainOutcome {
					return failf("%s: code %s, want uncertain_outcome: the host processed the commit and only the reply is unusable", what, code(o.err))
				}
				if fx.Effects() != effects+1 {
					return failf("%s: the scenario requires the host to have applied the commit (effects %d -> %d)", what, effects, fx.Effects())
				}
			} else if code(o.err) != contracts.ErrSchemaInvalid {
				return failf("%s: code %s, want schema_invalid", what, code(o.err))
			}
			if n := len(newCalls(s, before)); n != 1 {
				return failf("%s reached the host %d times; an unusable reply must not be retried", what, n)
			}
			if err := stillWorks(ctx, s, what); err != nil {
				return err
			}
		}
	}

	// Describe, Health and Inspect replies are held to the same standard.
	for _, f := range []fake.Fault{fake.FaultMalformed, fake.FaultPartial, fake.FaultTextOnly} {
		fx.Inject(adapters.OpDescribe, f)
		d, err := describe(ctx, s)
		if err == nil || !contracts.HasCode(err, contracts.ErrSchemaInvalid) || len(d.Capabilities) != 0 {
			return failf("Describe with a %s reply: %+v, %v; want schema_invalid and nothing else", f, d, err)
		}
		fx.Inject(adapters.OpHealth, f)
		h, herr := health(ctx, s)
		if herr == nil && h.Status == adapters.HealthOK {
			return failf("Health with a %s reply reported ok", f)
		}
		fx.Inject(adapters.OpInspect, f)
		in, err := inspect(ctx, s, sc, adapters.InspectRequest{})
		if err == nil || !contracts.HasCode(err, contracts.ErrSchemaInvalid) || len(in.Reports) != 0 {
			return failf("Inspect with a %s reply: %+v, %v; want schema_invalid and nothing else", f, in, err)
		}
	}
	if _, err := describe(ctx, s); err != nil {
		return failf("after malformed describe, health and inspect replies Describe fails: %v", err)
	}
	return nil
}

func checkResourceBounds(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	tooLarge := func(what string, err error) error {
		if err == nil {
			return failf("%s: an oversize reply was accepted", what)
		}
		if !contracts.HasCode(err, contracts.ErrSchemaInvalid) || reasonIn(err) != adapters.ReasonResponseTooLarge {
			return failf("%s: %v, want schema_invalid with reason %s", what, err, adapters.ReasonResponseTooLarge)
		}
		return nil
	}
	within := func(what string, start time.Time) error {
		if d := time.Since(start); d > grace {
			return failf("%s took %s to refuse an oversize reply", what, d)
		}
		return nil
	}

	fx.Inject(adapters.OpRead, fake.FaultOversize)
	start := time.Now()
	o, err := readItem(ctx, s, sc, "item_1")
	if err != nil {
		return err
	}
	if err := tooLarge("read", o.err); err != nil {
		return err
	}
	if code(o.err) != contracts.ErrSchemaInvalid || !zeroResult(o.res) {
		return failf("read: oversize reply gave code %s and result %+v; want schema_invalid and no result", code(o.err), o.res)
	}
	if err := within("read", start); err != nil {
		return err
	}

	fx.Inject(adapters.OpDescribe, fake.FaultOversize)
	if _, err := describe(ctx, s); tooLarge("describe", err) != nil {
		return tooLarge("describe", err)
	}
	fx.Inject(adapters.OpInspect, fake.FaultOversize)
	if _, err := inspect(ctx, s, sc, adapters.InspectRequest{}); tooLarge("inspect", err) != nil {
		return tooLarge("inspect", err)
	}

	// The commit was processed; only the reply is oversize. The caller must be
	// told the outcome is unknown.
	p, err := prepared(ctx, s, sc, "Widget")
	if err != nil {
		return err
	}
	fx.Inject(adapters.OpCommit, fake.FaultOversize)
	effects := fx.Effects()
	start = time.Now()
	co, err := commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if err := tooLarge("commit", co.err); err != nil {
		return err
	}
	if code(co.err) != contracts.ErrUncertainOutcome {
		return failf("commit with an oversize reply: code %s, want uncertain_outcome", code(co.err))
	}
	if fx.Effects() != effects+1 {
		return failf("the scenario requires the host to have applied the commit")
	}
	if err := within("commit", start); err != nil {
		return err
	}
	so, err := status(ctx, s, sc, p.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || so.res.Status != actions.DeterministicResultStatusOk {
		return failf("status after an oversize commit reply: %s, %v; want ok", so.res.Status, so.err)
	}
	return stillWorks(ctx, s, "oversize replies")
}

func checkUnavailableDependency(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	unavailable := func(what string, o outcome) error {
		if o.err != nil {
			return failf("%s: a dependency reported down by the host came back as an error (%v); it is a typed result with status unavailable", what, o.err)
		}
		if o.res.Status != actions.DeterministicResultStatusUnavailable {
			return failf("%s: status %s, want unavailable (never a success-looking result)", what, o.res.Status)
		}
		if len(o.res.Errors) == 0 || issueCode(o.res) != "unavailable" {
			return failf("%s: unavailable without a typed unavailable error: %+v", what, o.res.Errors)
		}
		return nil
	}

	fx.Inject(adapters.OpRead, fake.FaultDependencyDown)
	o, err := readItem(ctx, s, sc, "item_1")
	if err != nil {
		return err
	}
	if err := unavailable("read", o); err != nil {
		return err
	}

	p, err := prepared(ctx, s, sc, "Widget")
	if err != nil {
		return err
	}
	fx.Inject(adapters.OpCommit, fake.FaultDependencyDown)
	before := callCount(s)
	co, err := commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if err := unavailable("commit", co); err != nil {
		return err
	}
	if len(co.res.Identifiers) != 0 {
		return failf("an unavailable commit reported records: %+v", co.res.Identifiers)
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("an unavailable commit reached the host %d times, want 1", n)
	}
	if fx.Effects() != 0 {
		return failf("an unavailable commit changed the application (%d effects)", fx.Effects())
	}
	so, err := status(ctx, s, sc, p.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || !adapters.StatusShowsNoRecord(so.res) {
		return failf("status after an unavailable commit: %s, %v; nothing was recorded, so it must be the no-record marker", so.res.Status, so.err)
	}
	// The host recovers; the same commit now goes through once.
	co, err = commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if co.err != nil || co.res.Status != actions.DeterministicResultStatusOk || fx.Effects() != 1 {
		return failf("the commit after the dependency recovered: %s, %v, %d effects", co.res.Status, co.err, fx.Effects())
	}
	return nil
}

func checkHealthAndRecovery(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	h, err := health(ctx, s)
	if err != nil || h.Status != adapters.HealthOK {
		return failf("Health of a running application: %+v, %v; want ok", h, err)
	}
	if err := h.Validate(); err != nil {
		return failf("Health returned an invalid report: %v", err)
	}

	fx.SetAvailable(false)
	defer fx.SetAvailable(true)
	h, err = health(ctx, s)
	if err == nil && h.Status != adapters.HealthUnavailable {
		return failf("Health of a down application: %+v; want unavailable", h)
	}
	if err != nil && code(err) != contracts.ErrUnavailable {
		return failf("Health of a down application failed with %v; want a report with status unavailable (or an unavailable error)", err)
	}
	p, perr := preparedWhileUp(ctx, s, sc, fx)
	if perr != nil {
		return perr
	}
	before := callCount(s)
	for _, c := range resultCalls(s, sc, p) {
		o, err := c.run(ctx)
		if err != nil {
			return err
		}
		if o.err == nil {
			return failf("%s to a down application returned %s", c.op, o.res.Status)
		}
		// Nothing was sent, so even a commit is a plain unavailable: only a
		// request that may have arrived is uncertain.
		if code(o.err) != contracts.ErrUnavailable || !code(o.err).Retryable() {
			return failf("%s to a down application: code %s (%v), want unavailable", c.op, code(o.err), o.err)
		}
	}
	if _, err := describe(ctx, s); code(err) != contracts.ErrUnavailable {
		return failf("Describe of a down application: %v, want unavailable", err)
	}
	if n := len(newCalls(s, before)); n != 0 {
		return failf("the host received %d requests while it was down", n)
	}

	fx.SetAvailable(true)
	h, err = health(ctx, s)
	if err != nil || h.Status != adapters.HealthOK {
		return failf("Health right after the application recovered: %+v, %v; want ok", h, err)
	}
	co, err := commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if co.err != nil || co.res.Status != actions.DeterministicResultStatusOk || fx.Effects() != 1 {
		return failf("the commit after the application recovered: %s, %v, %d effects; want ok and 1 effect (no sticky failure, no double apply)", co.res.Status, co.err, fx.Effects())
	}
	return nil
}

// preparedWhileUp prepares an action while the host is reachable, then takes it
// down again, so the down-host calls have a real prepared action to send.
func preparedWhileUp(ctx context.Context, s Subject, sc contracts.Scope, fx Fixture) (actions.PreparedAction, error) {
	fx.SetAvailable(true)
	p, err := prepared(ctx, s, sc, "Widget")
	fx.SetAvailable(false)
	return p, err
}

func checkInspect(ctx context.Context, s Subject) error {
	sc := scopeA(s)
	in, err := inspect(ctx, s, sc, adapters.InspectRequest{})
	if err != nil {
		return failf("Inspect failed: %v", err)
	}
	all := adapters.AllAspects()
	if len(in.Reports) != len(all) {
		return failf("Inspect returned %d reports for a request for every aspect (%d)", len(in.Reports), len(all))
	}
	reported := map[adapters.Aspect]bool{adapters.AspectVersions: true, adapters.AspectHealth: true, adapters.AspectLimits: true}
	var wantUnverified []adapters.Aspect
	for i, a := range all {
		r := in.Reports[i]
		if r.Aspect != a {
			return failf("report %d is for %s, want %s (canonical order)", i, r.Aspect, a)
		}
		if reported[a] {
			if r.State != adapters.AspectReported || len(r.Facts) == 0 {
				return failf("aspect %s: state %s with %d facts; the application exposes it, so it must be reported with facts", a, r.State, len(r.Facts))
			}
			continue
		}
		wantUnverified = append(wantUnverified, a)
		if r.State != adapters.AspectUnsupported || len(r.Facts) != 0 {
			return failf("aspect %s: state %s with %d facts; an aspect the application does not expose is unsupported and carries nothing", a, r.State, len(r.Facts))
		}
	}
	if diff := cmp.Diff(wantUnverified, in.Unverified()); diff != "" {
		return failf("Unverified lists the wrong aspects (-want +got):\n%s", diff)
	}

	sub, err := inspect(ctx, s, sc, adapters.InspectRequest{Aspects: []adapters.Aspect{adapters.AspectNetwork, adapters.AspectVersions}})
	if err != nil {
		return failf("Inspect of a subset failed: %v", err)
	}
	if len(sub.Reports) != 2 || sub.Reports[0].Aspect != adapters.AspectVersions || sub.Reports[1].Aspect != adapters.AspectNetwork {
		return failf("Inspect of {network, versions} returned %+v, want versions then network", sub.Reports)
	}
	return nil
}
