package adaptersuite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

const (
	tenantA, workspaceA, principalA = "ten_a", "wks_a", "usr_alice"
	tenantB, workspaceB, principalB = "ten_b", "wks_b", "usr_bob"
)

// grace is how long past its context an adapter gets to return before the
// suite declares that it ignores the context. It is generous because CI
// machines are slow; a correct adapter returns in milliseconds. It is a
// variable so the suite's own tests, which run deliberately broken adapters,
// can shorten it.
var grace = 5 * time.Second

func scopeFor(s Subject, tenant, workspace, principal string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_conformance",
		ApplicationID:    s.ApplicationID,
		TenantID:         tenant,
		WorkspaceID:      workspace,
		PrincipalID:      principal,
		Roles:            []string{"member", "reviewer"},
		CapabilityScopes: []string{fake.ReadCapability, fake.ActionCapability},
		SessionID:        "ses_" + principal,
	}
}

func scopeA(s Subject) contracts.Scope { return scopeFor(s, tenantA, workspaceA, principalA) }
func scopeB(s Subject) contracts.Scope { return scopeFor(s, tenantB, workspaceB, principalB) }

func newKey() actions.IdempotencyKey { return actions.IdempotencyKey(ids.New("idem")) }

func failf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// reply is what a bounded call sends back from its goroutine.
type reply[T any] struct {
	v   T
	err error
}

// bounded runs fn in its own goroutine and returns what it sent back, or a
// failure if fn has not returned once ctx is done and the grace period has
// passed: the adapter ignores its context. The goroutine writes nothing the
// caller can see; it only sends its result on a buffered channel, so a call
// that outlives its watchdog (an adapter that ignores ctx) cannot race with the
// check that gave up on it.
func bounded[T any](ctx context.Context, what string, fn func() (T, error)) (reply[T], error) {
	done := make(chan reply[T], 1)
	go func() {
		v, err := fn()
		done <- reply[T]{v, err}
	}()
	select {
	case r := <-done:
		return r, nil
	case <-ctx.Done():
	}
	select {
	case r := <-done:
		return r, nil
	case <-time.After(grace):
		return reply[T]{}, failf("%s: the adapter did not return within %s after its context ended (it must honour ctx)", what, grace)
	}
}

// outcome is the result of one adapter call.
type outcome struct {
	res actions.DeterministicResult
	err error
}

// callResult runs a result-returning call under ctx with the watchdog. The
// returned error is the watchdog's; the adapter's own error is outcome.err.
func callResult(ctx context.Context, what string, fn func(context.Context) (actions.DeterministicResult, error)) (outcome, error) {
	r, werr := bounded(ctx, what, func() (actions.DeterministicResult, error) { return fn(ctx) })
	if werr != nil {
		return outcome{}, werr
	}
	return outcome{res: r.v, err: r.err}, nil
}

func code(err error) contracts.ErrCode { return contracts.CodeOf(err) }

// reasonIn finds Details["reason"] on any *contracts.Error in the chain.
func reasonIn(err error) string {
	for err != nil {
		var ce *contracts.Error
		if !errors.As(err, &ce) {
			return ""
		}
		if r, ok := ce.Details["reason"].(string); ok {
			return r
		}
		err = ce.Unwrap()
	}
	return ""
}

// newCalls returns the host calls recorded after the first n.
func newCalls(s Subject, n int) []fake.Call {
	all := s.Fixture.Calls()
	if len(all) <= n {
		return nil
	}
	return all[n:]
}

func callCount(s Subject) int { return len(s.Fixture.Calls()) }

// zeroResult reports whether a result is the zero value, as an adapter must
// return alongside an error.
func zeroResult(r actions.DeterministicResult) bool {
	return r.Status == "" && r.Operation == "" && r.CapabilityID == "" &&
		len(r.Identifiers) == 0 && len(r.Values) == 0 && len(r.Errors) == 0 && r.Preparation == nil
}

// issueCode returns the code of the first issue of a result, or "".
func issueCode(r actions.DeterministicResult) string {
	if len(r.Errors) == 0 {
		return ""
	}
	return string(r.Errors[0].Code)
}

func issueField(r actions.DeterministicResult) string {
	if len(r.Errors) == 0 || r.Errors[0].Field == nil {
		return ""
	}
	return string(*r.Errors[0].Field)
}

// validate calls Validate on the action capability.
func validateName(ctx context.Context, s Subject, scope contracts.Scope, fields map[string]any) (outcome, error) {
	return callResult(ctx, "validate", func(c context.Context) (actions.DeterministicResult, error) {
		return s.Adapter.Validate(c, scope, adapters.ValidateRequest{CapabilityID: fake.ActionCapability, Fields: fields})
	})
}

func readItem(ctx context.Context, s Subject, scope contracts.Scope, id string) (outcome, error) {
	return callResult(ctx, "read", func(c context.Context) (actions.DeterministicResult, error) {
		return s.Adapter.Read(c, scope, adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": id}})
	})
}

func prepareName(ctx context.Context, s Subject, scope contracts.Scope, name string) (outcome, error) {
	return callResult(ctx, "prepare", func(c context.Context) (actions.DeterministicResult, error) {
		return s.Adapter.Prepare(c, scope, adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": name}})
	})
}

func commit(ctx context.Context, s Subject, scope contracts.Scope, p actions.PreparedAction) (outcome, error) {
	return callResult(ctx, "commit", func(c context.Context) (actions.DeterministicResult, error) {
		return s.Adapter.Commit(c, scope, adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
	})
}

func status(ctx context.Context, s Subject, scope contracts.Scope, key actions.IdempotencyKey) (outcome, error) {
	return callResult(ctx, "status", func(c context.Context) (actions.DeterministicResult, error) {
		return s.Adapter.Status(c, scope, adapters.StatusRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: key})
	})
}

func cancelKey(ctx context.Context, s Subject, scope contracts.Scope, key actions.IdempotencyKey) (outcome, error) {
	return callResult(ctx, "cancel", func(c context.Context) (actions.DeterministicResult, error) {
		return s.Adapter.Cancel(c, scope, adapters.CancelRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: key})
	})
}

// prepared runs Prepare and builds the complete PreparedAction the coordinator
// would hold for the result, with a fresh idempotency key.
func prepared(ctx context.Context, s Subject, scope contracts.Scope, name string) (actions.PreparedAction, error) {
	o, err := prepareName(ctx, s, scope, name)
	if err != nil {
		return actions.PreparedAction{}, err
	}
	if o.err != nil {
		return actions.PreparedAction{}, failf("prepare %q failed: %v", name, o.err)
	}
	if o.res.Status != actions.DeterministicResultStatusOk || o.res.Preparation == nil {
		return actions.PreparedAction{}, failf("prepare %q: status %s without a usable preparation (%+v)", name, o.res.Status, o.res.Errors)
	}
	return preparedFrom(o.res), nil
}

// preparedFrom builds a PreparedAction from a successful Prepare result.
func preparedFrom(res actions.DeterministicResult) actions.PreparedAction {
	p := res.Preparation
	return actions.PreparedAction{
		PreparationID:          actions.PreparationID(ids.New("prep")),
		CapabilityID:           res.CapabilityID,
		State:                  actions.PreparedActionStateCommitting,
		Summary:                p.Summary,
		Effects:                p.Effects,
		RiskLevel:              p.DeclaredRiskLevel,
		DataVersions:           p.DataVersions,
		PreparedAt:             "2026-10-09T08:00:00Z",
		ExpiresAt:              "2026-10-09T08:05:00Z",
		InvalidationConditions: []actions.PreparedActionInvalidationConditionsItem{actions.PreparedActionInvalidationConditionsItemDataVersionChanged},
		Confirmation:           actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone, Bindings: []actions.ConfirmationRequirementBindingsItem{}},
		IdempotencyKey:         newKey(),
	}
}

// committed prepares and commits one item and returns its record ID.
func committed(ctx context.Context, s Subject, scope contracts.Scope, name string) (id string, p actions.PreparedAction, err error) {
	p, err = prepared(ctx, s, scope, name)
	if err != nil {
		return "", p, err
	}
	o, err := commit(ctx, s, scope, p)
	if err != nil {
		return "", p, err
	}
	if o.err != nil || o.res.Status != actions.DeterministicResultStatusOk || len(o.res.Identifiers) != 1 {
		return "", p, failf("commit %q: status %s, identifiers %d, error %v", name, o.res.Status, len(o.res.Identifiers), o.err)
	}
	return string(o.res.Identifiers[0].EntityID), p, nil
}
