package modes

import (
	"context"
	"errors"
	"testing"

	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// adapterErr is an error as an external adapter returns it: a typed code with the two details every adapter sets.
func adapterErr(code contracts.ErrCode, kind pv.ProviderErrorKind, sent bool) error {
	e := contracts.New(code, "adapter: fixed text")
	if kind != "" {
		e = e.WithDetails("provider_error_kind", string(kind))
	}
	return e.WithDetails("sent", sent)
}

// ---- G14: a credential refusal is never an uncertain outcome ----

func TestCredentialRefusalIsNeverAnUncertainOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// raw: the router also reads this error right when a sender returns it as it is, because it proves
		// nothing was sent. Every case is also run through the mapper the senders use.
		raw bool
	}{
		{"scope violation of the resolver, sent=false, kind named", adapterErr(contracts.ErrScopeViolation, pv.ProviderErrorKindAuthenticationFailed, false), true},
		{"scope violation of the resolver, sent=false, no kind", adapterErr(contracts.ErrScopeViolation, "", false), true},
		{"scope missing, sent=false, no kind", adapterErr(contracts.ErrScopeMissing, "", false), true},
		{"unauthenticated, sent=false, no kind", adapterErr(contracts.ErrUnauthenticated, "", false), true},
		{"permission denied by the resolver, sent=false", adapterErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, false), true},
		{"a credential refusal that says nothing about sent", contracts.New(contracts.ErrScopeViolation, "credentials: not this scope's"), false},
		{"the provider's own 401, sent=true", adapterErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true), false},
	}
	for _, c := range cases {
		for _, via := range []string{"mapped", "raw"} {
			if via == "raw" && !c.raw {
				continue
			}
			t.Run(c.name+" ("+via+")", func(t *testing.T) {
				r := newRig(t, ModeExternal)
				r.profiles.fallbackTo = profB
				r.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondUnavailable, CondTimeout}}
				r.sendErr[profA] = c.err
				if via == "mapped" {
					r.sendErr[profA] = CallErrorOf(c.err)
				}
				req := r.request(RoleController)
				req.CommitCapable = true // the case that turns a lost send into uncertain_outcome
				o, err := r.execute(req)

				mustKind(t, o, err, KindInvalidCredential)
				if contracts.CodeOf(err) == contracts.ErrUncertainOutcome {
					t.Fatalf("code %s", contracts.CodeOf(err))
				}
				if r.count("send:") != 1 || r.count("fallback:") != 0 || r.count("local") != 0 {
					t.Fatalf("one send, and neither another provider nor the local runtime may follow: %v", r.log)
				}
				got := r.budgets.settled
				if len(got) != 1 || got[0].Outcome != pv.UsageRecordOutcomeFailed || got[0].ErrorKind == nil || *got[0].ErrorKind != pv.ProviderErrorKindAuthenticationFailed {
					t.Fatalf("the budget must settle as failed with authentication_failed, never uncertain (the reserved bound): %+v", got)
				}
				if got[0].Metering != (providerbudget.Units{}) {
					t.Fatalf("a refused credential consumed nothing: %+v", got[0].Metering)
				}
			})
		}
	}

	t.Run("controls: a lost connection after the send is still uncertain", func(t *testing.T) {
		for name, err := range map[string]error{
			"a bare network error":      errors.New("read tcp: connection reset"),
			"an adapter error, sent":    adapterErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, true),
			"a typed error, no details": contracts.New(contracts.ErrInternal, "x"),
		} {
			r := newRig(t, ModeExternal)
			r.sendErr[profA] = err
			req := r.request(RoleController)
			req.CommitCapable = true
			o, rerr := r.execute(req)
			if o.Kind != KindUncertain {
				t.Fatalf("%s: outcome %s (%v), want uncertain_outcome", name, o.Kind, rerr)
			}
			if got := r.budgets.settled; len(got) != 1 || got[0].Outcome != pv.UsageRecordOutcomeUncertain {
				t.Fatalf("%s: settled %+v", name, got)
			}
		}
	})

	t.Run("the mapper is exact about sent", func(t *testing.T) {
		for _, c := range []struct {
			err      error
			wantKind pv.ProviderErrorKind
			wantSent bool
		}{
			{adapterErr(contracts.ErrScopeViolation, pv.ProviderErrorKindAuthenticationFailed, false), pv.ProviderErrorKindAuthenticationFailed, false},
			{adapterErr(contracts.ErrScopeViolation, "", false), pv.ProviderErrorKindAuthenticationFailed, false},
			{contracts.New(contracts.ErrScopeViolation, "x"), pv.ProviderErrorKindAuthenticationFailed, true}, // nothing says it did not leave
			{adapterErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true), pv.ProviderErrorKindAuthenticationFailed, true},
			{adapterErr(contracts.ErrTimeout, pv.ProviderErrorKindTimeout, true), pv.ProviderErrorKindTimeout, true},
			{adapterErr(contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, false), pv.ProviderErrorKindBadRequest, false},
			{contracts.New(contracts.ErrValidationFailed, "x"), pv.ProviderErrorKindBadRequest, false},
			{errors.New("reset"), pv.ProviderErrorKindConnectionLost, true},
			{adapterErr(contracts.ErrUnavailable, pv.ProviderErrorKind("not_a_kind"), true), pv.ProviderErrorKindUnavailable, true},
		} {
			ce := CallErrorOf(c.err)
			if ce.Kind != c.wantKind || ce.Sent != c.wantSent {
				t.Errorf("%v: kind %s sent %v, want %s %v", c.err, ce.Kind, ce.Sent, c.wantKind, c.wantSent)
			}
			if ce.Err != nil {
				t.Errorf("%v: the provider's error is carried in the CallError", c.err)
			}
		}
	})
}

// ---- G15: the kill switch is read again after the reservation ----

// engagingBudgets engages the kill switch right after it reserved: the window between the reservation and the send.
type engagingBudgets struct {
	*fakeBudgets
	ks *fakeKS
}

func (b *engagingBudgets) Reserve(ctx context.Context, s contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	rsv, err := b.fakeBudgets.Reserve(ctx, s, c)
	b.ks.set(true)
	return rsv, err
}

// releasingBudgets is a budget that can give a reservation back.
type releasingBudgets struct {
	*engagingBudgets
	released []pv.ReservationID
	err      error
}

func (b *releasingBudgets) Release(_ context.Context, _ contracts.Scope, id pv.ReservationID) error {
	b.rig.note("release")
	b.released = append(b.released, id)
	return b.err
}

type unreadableAfterReserve struct {
	*fakeBudgets
	ks *fakeKS
}

func (b *unreadableAfterReserve) Reserve(ctx context.Context, s contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	rsv, err := b.fakeBudgets.Reserve(ctx, s, c)
	b.ks.mu.Lock()
	b.ks.err = errors.New("store unavailable")
	b.ks.mu.Unlock()
	return rsv, err
}

func (r *rig) withBudgets(b Budgets) {
	rt, err := New(Config{Modes: r.modes, KillSwitch: r.ks, Policy: r.policy, Local: r.local, Profiles: r.profiles, Guard: r.guard, Budgets: b})
	if err != nil {
		r.t.Fatal(err)
	}
	r.router = rt
}

func TestKillSwitchEngagedAfterTheReservationStopsTheSend(t *testing.T) {
	t.Run("a budget that cannot release settles the reservation as cancelled", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		fb := &fakeBudgets{rig: r, states: []pv.BudgetState{{}}}
		r.withBudgets(&engagingBudgets{fakeBudgets: fb, ks: r.ks})
		o, err := r.execute(r.request(RoleController))

		mustKind(t, o, err, KindServed)
		if r.count("send:") != 0 {
			t.Fatalf("the request was sent after the switch was engaged: %v", r.log)
		}
		if o.Target != TargetLocal || o.DataSent || o.Reason != DecidedByKillSwitch {
			t.Fatalf("outcome %+v: served by the local provider, nothing sent, because of the switch", o)
		}
		if r.index("reserve:"+string(profA)) > r.index("local") || r.count("local") != 1 {
			t.Fatalf("the local provider serves after the reservation was given back: %v", r.log)
		}
		if len(fb.settled) != 1 || fb.settled[0].Outcome != pv.UsageRecordOutcomeCancelled || fb.settled[0].ErrorKind != nil || fb.settled[0].Metering != (providerbudget.Units{}) {
			t.Fatalf("the reservation must settle as cancelled with no metering: %+v", fb.settled)
		}
	})

	t.Run("a budget that can release gives the reservation back and charges nothing", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		fb := &fakeBudgets{rig: r, states: []pv.BudgetState{{}}}
		rb := &releasingBudgets{engagingBudgets: &engagingBudgets{fakeBudgets: fb, ks: r.ks}}
		r.withBudgets(rb)
		o, err := r.execute(r.request(RoleController))

		mustKind(t, o, err, KindServed)
		if r.count("send:") != 0 || len(rb.released) != 1 || rb.released[0] != "rsv_1" || len(fb.settled) != 0 {
			t.Fatalf("released %v settled %v: %v", rb.released, fb.settled, r.log)
		}
		if o.Target != TargetLocal || o.DataSent {
			t.Fatalf("outcome %+v", o)
		}
	})

	t.Run("a reservation that cannot be given back is reported, and still nothing is sent", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		fb := &fakeBudgets{rig: r, states: []pv.BudgetState{{}}}
		rb := &releasingBudgets{engagingBudgets: &engagingBudgets{fakeBudgets: fb, ks: r.ks}, err: errors.New("store down")}
		r.withBudgets(rb)
		o, _ := r.execute(r.request(RoleController))
		if r.count("send:") != 0 || !o.SettleFailed {
			t.Fatalf("outcome %+v: %v", o, r.log)
		}
	})

	t.Run("a switch that cannot be read at that point counts as engaged", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		fb := &fakeBudgets{rig: r, states: []pv.BudgetState{{}}}
		r.withBudgets(&unreadableAfterReserve{fakeBudgets: fb, ks: r.ks})
		o, err := r.execute(r.request(RoleController))
		mustKind(t, o, err, KindServed)
		if r.count("send:") != 0 || o.Target != TargetLocal {
			t.Fatalf("outcome %+v: %v", o, r.log)
		}
	})

	t.Run("control: with the switch released the request is sent", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		o, err := r.execute(r.request(RoleController))
		mustKind(t, o, err, KindServed)
		if r.count("send:") != 1 || o.Target != TargetExternal {
			t.Fatalf("outcome %+v: %v", o, r.log)
		}
	})
}

// ---- G13: the reply model is compared with the pinned model ----

// observingProfiles records what the router reports to the registry.
type observingProfiles struct {
	*fakeProfiles
	seen []providerreg.Observation
	err  error
}

func (p *observingProfiles) Observe(_ context.Context, _ contracts.Scope, id pv.ProfileID, obs providerreg.Observation) (pv.ProviderProfile, error) {
	p.rig.note("observe:" + string(id))
	p.seen = append(p.seen, obs)
	return pv.ProviderProfile{}, p.err
}

func (r *rig) withProfiles(p Profiles) {
	rt, err := New(Config{Modes: r.modes, KillSwitch: r.ks, Policy: r.policy, Local: r.local, Profiles: p, Guard: r.guard, Budgets: r.budgets})
	if err != nil {
		r.t.Fatal(err)
	}
	r.router = rt
}

func (r *rig) executeReply(model string) (Outcome, error) {
	r.t.Helper()
	return r.router.Execute(context.Background(), r.request(RoleController), Executors{
		Local: func(context.Context) error { r.note("local"); return nil },
		External: func(_ context.Context, c ExternalCall) (ExternalResult, error) {
			r.note("send:" + string(c.Admission.Profile.ProfileID))
			return ExternalResult{Metering: providerbudget.Units{ToksIn: 8, ToksOut: 4}, ReplyModel: model}, nil
		},
	})
}

func TestReplyModelMustMatchThePinnedModel(t *testing.T) {
	// profileOf pins acme / m1 / v1.
	t.Run("a reply of another model is discarded, billed as failed and quarantines the profile", func(t *testing.T) {
		for _, m := range []string{"m2", "m1-v2", "m", "m1-", "M1", "m1 ", "other/m1"} {
			r := newRig(t, ModeExternal)
			op := &observingProfiles{fakeProfiles: r.profiles}
			r.withProfiles(op)
			o, err := r.executeReply(m)

			mustKind(t, o, err, KindMalformedOutput)
			if o.Reason != "reply_model_mismatch" || o.Served() || o.Target == TargetExternal {
				t.Fatalf("%q: outcome %+v", m, o)
			}
			if len(op.seen) != 1 || op.seen[0].ReplyModel == nil || *op.seen[0].ReplyModel != m {
				t.Fatalf("%q: the registry was told %+v", m, op.seen)
			}
			got := r.budgets.settled
			if len(got) != 1 || got[0].Outcome != pv.UsageRecordOutcomeFailed || got[0].ErrorKind == nil || *got[0].ErrorKind != pv.ProviderErrorKindMalformedResponse {
				t.Fatalf("%q: settled %+v", m, got)
			}
			if got[0].Metering.ToksIn != 8 || got[0].Metering.ToksOut != 4 {
				t.Fatalf("%q: the call happened, so what the provider reported is billed: %+v", m, got[0].Metering)
			}
			if r.count("send:") != 1 || r.count("fallback:") != 0 {
				t.Fatalf("%q: %v", m, r.log)
			}
		}
	})

	t.Run("the pinned id and its snapshots are served, and the registry is not written on the hot path", func(t *testing.T) {
		for _, m := range []string{"m1", "m1-v1", ""} {
			r := newRig(t, ModeExternal)
			op := &observingProfiles{fakeProfiles: r.profiles}
			r.withProfiles(op)
			o, err := r.executeReply(m)
			mustKind(t, o, err, KindServed)
			if len(op.seen) != 0 {
				t.Fatalf("%q: observed %+v", m, op.seen)
			}
		}
	})

	t.Run("a mismatching reply is refused even when the quarantine cannot be recorded, and it is reported", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		r.withProfiles(&observingProfiles{fakeProfiles: r.profiles, err: errors.New("store down")})
		o, err := r.executeReply("m2")
		mustKind(t, o, err, KindMalformedOutput)
		if !o.QuarantineFailed {
			t.Fatalf("outcome %+v", o)
		}

		r = newRig(t, ModeExternal) // a registry that cannot observe at all
		o, err = r.executeReply("m2")
		mustKind(t, o, err, KindMalformedOutput)
		if !o.QuarantineFailed {
			t.Fatalf("outcome %+v", o)
		}
	})

	t.Run("the local fallback of malformed output serves a request whose reply was discarded", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		r.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondMalformedOutput}}
		r.withProfiles(&observingProfiles{fakeProfiles: r.profiles})
		o, err := r.executeReply("m2")
		mustKind(t, o, err, KindServed)
		if o.Target != TargetLocal || len(o.Hops) != 1 || o.Hops[0].Condition != CondMalformedOutput {
			t.Fatalf("outcome %+v", o)
		}
	})
}
