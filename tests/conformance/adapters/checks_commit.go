package adaptersuite

import (
	"context"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func checkHostDenial(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	// The host's reauthorisation refuses a read, a prepare and a commit. Each is
	// a rejected result with permission_denied, reaches the host once and is
	// never retried or worked around.
	fx.Inject(adapters.OpRead, fake.FaultDeny)
	before := callCount(s)
	o, err := readItem(ctx, s, sc, "item_1")
	if err != nil {
		return err
	}
	if o.err != nil || o.res.Status != actions.DeterministicResultStatusRejected || issueCode(o.res) != "permission_denied" {
		return failf("a denied read: status %s, issue %s, error %v; want a rejected result with permission_denied", o.res.Status, issueCode(o.res), o.err)
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("a denied read reached the host %d times, want 1", n)
	}

	fx.Inject(adapters.OpPrepare, fake.FaultDeny)
	before = callCount(s)
	po, err := prepareName(ctx, s, sc, "Widget")
	if err != nil {
		return err
	}
	if po.err != nil || po.res.Status != actions.DeterministicResultStatusRejected || issueCode(po.res) != "permission_denied" || po.res.Preparation != nil {
		return failf("a denied prepare: status %s, issue %s, preparation %v, error %v", po.res.Status, issueCode(po.res), po.res.Preparation, po.err)
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("a denied prepare reached the host %d times, want 1", n)
	}

	p, err := prepared(ctx, s, sc, "Widget")
	if err != nil {
		return err
	}
	fx.Inject(adapters.OpCommit, fake.FaultDeny)
	before = callCount(s)
	co, err := commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if co.err != nil || co.res.Status != actions.DeterministicResultStatusRejected || issueCode(co.res) != "permission_denied" {
		return failf("a denied commit: status %s, issue %s, error %v; want a rejected result with permission_denied (never a success, never an uncertain outcome)", co.res.Status, issueCode(co.res), co.err)
	}
	if len(co.res.Identifiers) != 0 {
		return failf("a denied commit reported records: %+v", co.res.Identifiers)
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("a denied commit reached the host %d times, want 1: the adapter must not retry around a denial", n)
	}
	if n := fx.Effects(); n != 0 {
		return failf("a denied commit changed the application (%d effects)", n)
	}
	so, err := status(ctx, s, sc, p.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || !adapters.StatusShowsNoRecord(so.res) {
		return failf("status after a denied commit: %s %+v, %v; the host recorded nothing, so it must answer with the no-record marker", so.res.Status, so.res.Errors, so.err)
	}

	// A denial is not sticky: the same commit, allowed, goes through once.
	co, err = commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if co.err != nil || co.res.Status != actions.DeterministicResultStatusOk || fx.Effects() != 1 {
		return failf("the commit after a denial: status %s, error %v, %d effects; want ok and exactly 1 effect", co.res.Status, co.err, fx.Effects())
	}

	// A denied Status call says nothing about the commit, which was applied: it
	// is a rejected result, but it is not the no-record marker, so it can never
	// be read as permission to commit again.
	fx.Inject(adapters.OpStatus, fake.FaultDeny)
	before = callCount(s)
	so, err = status(ctx, s, sc, p.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || so.res.Status != actions.DeterministicResultStatusRejected || issueCode(so.res) != "permission_denied" {
		return failf("a denied status: status %s, issue %s, error %v; want a rejected result with permission_denied", so.res.Status, issueCode(so.res), so.err)
	}
	if adapters.StatusShowsNoRecord(so.res) {
		return failf("a denied status was read as the no-record marker: the commit was applied, and this would invite a second commit")
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("a denied status reached the host %d times, want 1", n)
	}
	return nil
}

func checkIdempotentCommit(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	p1, err := prepared(ctx, s, sc, "First")
	if err != nil {
		return err
	}
	first, err := commit(ctx, s, sc, p1)
	if err != nil {
		return err
	}
	if first.err != nil || first.res.Status != actions.DeterministicResultStatusOk || len(first.res.Identifiers) != 1 {
		return failf("the first commit: status %s, error %v", first.res.Status, first.err)
	}
	if fx.Effects() != 1 {
		return failf("one commit applied %d effects", fx.Effects())
	}

	// The same key again, as a retry or a double submit would send it.
	for i := 0; i < 2; i++ {
		repeat, err := commit(ctx, s, sc, p1)
		if err != nil {
			return err
		}
		if repeat.err != nil {
			return failf("a repeated commit with the same key failed: %v", repeat.err)
		}
		if diff := cmp.Diff(first.res, repeat.res); diff != "" {
			return failf("a repeated commit with the same key did not return the original result (-first +repeat):\n%s", diff)
		}
		if n := fx.Effects(); n != 1 {
			return failf("repeat %d of the same key applied the effect again (%d effects in total): commit is not idempotent", i+1, n)
		}
	}
	so, err := status(ctx, s, sc, p1.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || so.res.Status != actions.DeterministicResultStatusOk || len(so.res.Identifiers) != 1 || so.res.Identifiers[0] != first.res.Identifiers[0] {
		return failf("status of the key: %s, %v; want ok with the committed record", so.res.Status, so.err)
	}

	// A different key is a different action and is applied.
	p2, err := prepared(ctx, s, sc, "Second")
	if err != nil {
		return err
	}
	second, err := commit(ctx, s, sc, p2)
	if err != nil {
		return err
	}
	if second.err != nil || second.res.Status != actions.DeterministicResultStatusOk || fx.Effects() != 2 {
		return failf("a second action with its own key: status %s, error %v, %d effects; want ok and 2 effects", second.res.Status, second.err, fx.Effects())
	}
	if second.res.Identifiers[0] == first.res.Identifiers[0] {
		return failf("two different keys produced the same record %v", second.res.Identifiers[0])
	}

	// The first key still answers with the first result.
	again, err := commit(ctx, s, sc, p1)
	if err != nil {
		return err
	}
	if again.err != nil || cmp.Diff(first.res, again.res) != "" || fx.Effects() != 2 {
		return failf("the first key after another commit: %v, %d effects; want the original result and no new effect", again.err, fx.Effects())
	}
	return nil
}

func checkStatusAfterUncertainCommit(ctx context.Context, s Subject) error {
	fx := s.Fixture
	sc := scopeA(s)

	uncertain := func(what string, o outcome) error {
		if o.err == nil {
			return failf("%s: the commit reported %s; it must be an error", what, o.res.Status)
		}
		if code(o.err) != contracts.ErrUncertainOutcome {
			return failf("%s: code %s (%v), want uncertain_outcome: a commit that may have been applied must not look like a plain failure", what, code(o.err), o.err)
		}
		if code(o.err).Retryable() {
			return failf("%s: uncertain_outcome must not be retryable", what)
		}
		if !zeroResult(o.res) {
			return failf("%s: a result came back with the uncertain error", what)
		}
		return nil
	}

	// A. The host applied the commit, the reply was lost.
	pA, err := prepared(ctx, s, sc, "Reply lost")
	if err != nil {
		return err
	}
	fx.Inject(adapters.OpCommit, fake.FaultDropReply)
	before := callCount(s)
	o, err := commit(ctx, s, sc, pA)
	if err != nil {
		return err
	}
	if err := uncertain("reply lost after the host applied the commit", o); err != nil {
		return err
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("an uncertain commit reached the host %d times, want 1: the adapter must not retry a commit", n)
	}
	if fx.Effects() != 1 {
		return failf("the host applied %d effects, the scenario needs exactly 1", fx.Effects())
	}
	so, err := status(ctx, s, sc, pA.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || so.res.Status != actions.DeterministicResultStatusOk || len(so.res.Identifiers) != 1 {
		return failf("status after an applied-but-unconfirmed commit: %s, %v; want ok with the record", so.res.Status, so.err)
	}
	retry, err := commit(ctx, s, sc, pA)
	if err != nil {
		return err
	}
	if retry.err != nil || retry.res.Status != actions.DeterministicResultStatusOk || cmp.Diff(so.res.Identifiers, retry.res.Identifiers) != "" || fx.Effects() != 1 {
		return failf("the retry after status: %v, %d effects; want the original result and still 1 effect", retry.err, fx.Effects())
	}

	// B. The request never reached the host.
	pB, err := prepared(ctx, s, sc, "Request lost")
	if err != nil {
		return err
	}
	fx.Inject(adapters.OpCommit, fake.FaultDropRequest)
	before = callCount(s)
	o, err = commit(ctx, s, sc, pB)
	if err != nil {
		return err
	}
	if err := uncertain("request lost before the host processed it", o); err != nil {
		return err
	}
	if n := len(newCalls(s, before)); n != 1 {
		return failf("an uncertain commit reached the host %d times, want 1", n)
	}
	if fx.Effects() != 1 {
		return failf("the host applied a commit it never processed (%d effects)", fx.Effects())
	}
	so, err = status(ctx, s, sc, pB.IdempotencyKey)
	if err != nil {
		return err
	}
	if so.err != nil || !adapters.StatusShowsNoRecord(so.res) {
		return failf("status of a key the host never saw: status %s, issue %s on %q, error %v; want the no-record marker (rejected, validation_failed on idempotency_key)", so.res.Status, issueCode(so.res), issueField(so.res), so.err)
	}
	retry, err = commit(ctx, s, sc, pB)
	if err != nil {
		return err
	}
	if retry.err != nil || retry.res.Status != actions.DeterministicResultStatusOk || fx.Effects() != 2 {
		return failf("the retry after status showed nothing was applied: %v, %d effects; want ok and 2 effects", retry.err, fx.Effects())
	}
	return nil
}
