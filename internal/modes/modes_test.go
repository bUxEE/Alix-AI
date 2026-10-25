package modes

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ---- mode resolution --------------------------------------------------------------------------

func TestLocalIsDefaultMode(t *testing.T) {
	r := newRig(t, ModeLocal)
	res, err := r.router.Resolver().Resolve(context.Background(), testScope)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeLocal || res.DecidedBy != DecidedByDefault || res.KillSwitch {
		t.Fatalf("unset levels must resolve to local by default, got %+v", res)
	}
	// A role with an external route is still served locally and nothing external is touched.
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Target != TargetLocal || o.DataSent {
		t.Fatalf("local mode must serve locally and send nothing: %+v", o)
	}
	for _, p := range []string{"check:", "guard:", "reserve:", "send:", "fallback:"} {
		if r.count(p) != 0 {
			t.Fatalf("local mode touched %s: %v", p, r.log)
		}
	}
}

func TestMostRestrictiveModeWins(t *testing.T) {
	type lv = levelSetting
	set := func(m Mode) lv { return lv{m, true} }
	cases := []struct {
		name          string
		dep, app, ten lv
		want          Mode
		by            string
	}{
		{"all external", set(ModeExternal), set(ModeExternal), set(ModeExternal), ModeExternal, "deployment"},
		{"app narrows to hybrid", set(ModeExternal), set(ModeHybrid), set(ModeExternal), ModeHybrid, "application"},
		{"tenant narrows to local", set(ModeExternal), set(ModeExternal), set(ModeLocal), ModeLocal, "tenant"},
		{"tenant cannot widen", set(ModeLocal), set(ModeExternal), set(ModeExternal), ModeLocal, "deployment"},
		{"deployment hybrid, tenant external", set(ModeHybrid), set(ModeExternal), set(ModeExternal), ModeHybrid, "deployment"},
		{"unset deployment is local", lv{}, set(ModeExternal), set(ModeExternal), ModeLocal, DecidedByDefault},
		{"unset tenant is local", set(ModeExternal), set(ModeExternal), lv{}, ModeLocal, DecidedByDefault},
		{"invalid value is local", set(ModeExternal), set(Mode("turbo")), set(ModeExternal), ModeLocal, "application"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &rawModes{m: map[Level]lv{LevelDeployment: c.dep, LevelApplication: c.app, LevelTenant: c.ten}}
			res, err := NewResolver(src, &fakeKS{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := res.Resolve(context.Background(), testScope)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != c.want || got.DecidedBy != c.by {
				t.Fatalf("mode = %s by %s, want %s by %s", got.Mode, got.DecidedBy, c.want, c.by)
			}
		})
	}
	t.Run("unreadable level is local", func(t *testing.T) {
		res, _ := NewResolver(errModes{}, &fakeKS{})
		got, err := res.Resolve(context.Background(), testScope)
		if err != nil || got.Mode != ModeLocal || len(got.Unreadable) == 0 {
			t.Fatalf("an unreadable setting must resolve to local, got %+v, %v", got, err)
		}
	})
	t.Run("invalid scope", func(t *testing.T) {
		res, _ := NewResolver(NewMemoryModes(), &fakeKS{})
		if _, err := res.Resolve(context.Background(), contracts.Scope{}); err == nil {
			t.Fatal("a scope that does not validate must not resolve to a default")
		}
	})
}

type levelSetting struct {
	m   Mode
	set bool
}

type rawModes struct{ m map[Level]levelSetting }

func (s *rawModes) Mode(_ context.Context, l Level, _ contracts.Scope) (Mode, bool, error) {
	v := s.m[l]
	return v.m, v.set, nil
}

type errModes struct{}

func (errModes) Mode(context.Context, Level, contracts.Scope) (Mode, bool, error) {
	return "", false, errors.New("store down")
}

func TestModeSwitchDoesNotChangeHostContract(t *testing.T) {
	// The host sees the same result type, the same error type and the same detail keys in every mode.
	keys := func(err error) []string {
		var ce *contracts.Error
		if !errors.As(err, &ce) {
			t.Fatalf("not a contracts error: %v", err)
		}
		out := make([]string, 0, len(ce.Details))
		for k := range ce.Details {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	var servedRoles []Role
	var failKeys [][]string
	var failCodes []contracts.ErrCode
	for _, m := range []Mode{ModeLocal, ModeHybrid, ModeExternal} {
		ok := newRig(t, m)
		o, err := ok.execute(ok.request(RoleController))
		mustKind(t, o, err, KindServed)
		servedRoles = append(servedRoles, o.Role)

		bad := newRig(t, m)
		bad.local[RoleController] = false
		bad.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false)
		o, err = bad.execute(bad.request(RoleController))
		mustKind(t, o, err, KindUnavailable)
		failKeys = append(failKeys, keys(err))
		failCodes = append(failCodes, contracts.CodeOf(err))
	}
	for i := 1; i < 3; i++ {
		if servedRoles[i] != servedRoles[0] || !slices.Equal(failKeys[i], failKeys[0]) || failCodes[i] != failCodes[0] {
			t.Fatalf("the host contract differs between modes: %v %v %v", servedRoles, failKeys, failCodes)
		}
	}
}

// ---- the gates before an external call --------------------------------------------------------

func TestRouterAppliesEgressGuardBeforeCall(t *testing.T) {
	r := newRig(t, ModeExternal)
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindServed)
	if !(r.index("check:"+string(profA)) < r.index("guard:"+string(profA)) && r.index("guard:"+string(profA)) < r.index("send:"+string(profA))) {
		t.Fatalf("the guard must run after the profile gate and before the call: %v", r.log)
	}
	if r.guard.last.Role != pv.RoleConversationalController || r.guard.last.Profile.ProfileID != profA || r.guard.last.Scope.TenantID != "ten1" {
		t.Fatalf("the guard got the wrong candidate: %+v", r.guard.last)
	}

	d := newRig(t, ModeExternal)
	d.guard.deny = contracts.New(contracts.ErrPermissionDenied, "egress denied").WithDetails("rule", "classification")
	o, err = d.execute(d.request(RoleController))
	mustKind(t, o, err, KindEgressDenied)
	if d.count("reserve:")+d.count("send:") != 0 || o.DataSent || o.Reason != "classification" {
		t.Fatalf("a refused egress must reserve nothing and send nothing: %v %+v", d.log, o)
	}
}

func TestRouterChecksBudgetBeforeCall(t *testing.T) {
	r := newRig(t, ModeExternal)
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindServed)
	if !(r.index("guard:"+string(profA)) < r.index("reserve:"+string(profA)) && r.index("reserve:"+string(profA)) < r.index("send:"+string(profA)) && r.index("send:"+string(profA)) < r.index("settle:succeeded")) {
		t.Fatalf("expected guard, reserve, send, settle in that order: %v", r.log)
	}
	c := r.budgets.calls[0]
	if c.EnvelopeID != "envl_1" || c.DecisionID != "edec_1" || c.EstimateFloor != 100 || c.Units.ToksIn != 10 || c.Role != pv.RoleConversationalController || c.Channel != pv.ChannelInteractive {
		t.Fatalf("the reservation must carry the guard's ids, the estimate and the role: %+v", c)
	}

	for _, reason := range []string{"no_budget", "budget_exhausted"} {
		b := newRig(t, ModeExternal)
		b.profiles.fallbackTo = profB // a fallback exists; an exhausted budget must never use it
		b.budgets.reserve = contracts.New(contracts.ErrResourceExhausted, "refused").WithDetails("reason", reason)
		o, err = b.execute(b.request(RoleController))
		mustKind(t, o, err, KindBudgetExhausted)
		if b.count("send:") != 0 || b.count("fallback:") != 0 || b.count("local") != 0 || len(o.Hops) != 0 || o.Reason != reason {
			t.Fatalf("%s: no call, no fallback: %v %+v", reason, b.log, o)
		}
	}
	rl := newRig(t, ModeExternal)
	rl.budgets.reserve = contracts.New(contracts.ErrRateLimited, "slow").WithDetails("reason", "rate_limited").WithDetails("retry_after_ms", int64(1500))
	o, err = rl.execute(rl.request(RoleController))
	mustKind(t, o, err, KindRateLimited)
	if o.RetryAfter != 1500*time.Millisecond || rl.count("send:") != 0 {
		t.Fatalf("rate limit: %+v %v", o, rl.log)
	}
}

// ---- no hidden fallback -----------------------------------------------------------------------

func TestInvalidCredentialNoHiddenFallback(t *testing.T) {
	r := newRig(t, ModeExternal)
	r.profiles.fallbackTo = profB
	r.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA, profB}}
	r.profiles.checkErr[profA] = reasonErr(contracts.ErrPermissionDenied, providerreg.ReasonCredentialInvalid)
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindInvalidCredential)
	if r.count("check:") != 1 || r.count("fallback:") != 0 || r.count("guard:")+r.count("send:")+r.count("local") != 0 || len(o.Hops) != 0 {
		t.Fatalf("an invalid credential must not select the next candidate, a fallback or local: %v", r.log)
	}

	// The provider rejecting the credential at call time is the same.
	s := newRig(t, ModeExternal)
	s.profiles.fallbackTo = profB
	s.sendErr[profA] = callErr(pv.ProviderErrorKindAuthenticationFailed, true)
	o, err = s.execute(s.request(RoleController))
	mustKind(t, o, err, KindInvalidCredential)
	if s.count("send:") != 1 || s.count("fallback:") != 0 || s.count("local") != 0 {
		t.Fatalf("a rejected credential must not fall back: %v", s.log)
	}
	if got := s.budgets.settled[0]; got.Outcome != pv.UsageRecordOutcomeFailed {
		t.Fatalf("a rejected call settles as failed, got %s", got.Outcome)
	}
}

func TestStaleProfileQuarantinedNoFallback(t *testing.T) {
	for _, reason := range []providerreg.Reason{providerreg.ReasonStaleReview, providerreg.ReasonTermsChanged, providerreg.ReasonNotQualified} {
		r := newRig(t, ModeExternal)
		r.profiles.fallbackTo = profB
		r.profiles.checkErr[profA] = reasonErr(contracts.ErrQuarantined, reason)
		o, err := r.execute(r.request(RoleController))
		mustKind(t, o, err, KindStaleProfile)
		if o.Reason != string(reason) || contracts.CodeOf(err).Retryable() {
			t.Fatalf("%s: reason %q code %s", reason, o.Reason, contracts.CodeOf(err))
		}
		if r.count("fallback:")+r.count("guard:")+r.count("send:")+r.count("local") != 0 {
			t.Fatalf("%s: a quarantined profile is never used and never replaced: %v", reason, r.log)
		}
	}
}

func TestEgressDeniedKeepsDataLocal(t *testing.T) {
	r := newRig(t, ModeExternal)
	r.guard.deny = contracts.New(contracts.ErrPermissionDenied, "egress denied").WithDetails("rule", "classification")
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindEgressDenied)
	if o.DataSent || r.count("send:") != 0 || r.count("reserve:") != 0 || r.count("local") != 0 {
		t.Fatalf("denied egress keeps the data local and, unnamed, serves nothing: %v %+v", r.log, o)
	}

	// With the local provider named for that condition it serves, and the hop is recorded.
	n := newRig(t, ModeExternal)
	n.guard.deny = contracts.New(contracts.ErrPermissionDenied, "egress denied").WithDetails("rule", "classification")
	n.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondEgressDenied}}
	o, err = n.execute(n.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Target != TargetLocal || o.DataSent || len(o.Hops) != 1 || o.Hops[0].To != LocalHop || o.Hops[0].Condition != CondEgressDenied || n.count("send:") != 0 {
		t.Fatalf("named local fallback after a denial: %+v %v", o, n.log)
	}

	// A guard that fails (not just refuses) is a denial too, never a pass.
	f := newRig(t, ModeExternal)
	f.guard.deny = errors.New("store down")
	o, err = f.execute(f.request(RoleController))
	mustKind(t, o, err, KindEgressDenied)
}

func TestFallbackOnlyWhenPolicyNamesIt(t *testing.T) {
	// Nothing named: the failure ends the request.
	r := newRig(t, ModeExternal)
	r.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false)
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindUnavailable)
	if r.count("local") != 0 || len(o.Hops) != 0 || r.count("send:") != 1 || !o.AIActionsDisabled {
		t.Fatalf("no fallback was named: %v %+v", r.log, o)
	}

	// The profile names a fallback (providerreg grants it): one more attempt through every gate.
	p := newRig(t, ModeExternal)
	p.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false)
	p.profiles.fallbackTo = profB
	o, err = p.execute(p.request(RoleController))
	mustKind(t, o, err, KindServed)
	if len(o.Hops) != 1 || o.Hops[0].From != string(profA) || o.Hops[0].To != string(profB) || o.Hops[0].Condition != CondUnavailable || o.Profile != profB {
		t.Fatalf("the hop must be recorded: %+v", o)
	}
	if p.count("guard:") != 2 || p.count("reserve:") != 2 || p.budgets.calls[1].FallbackFrom == nil || *p.budgets.calls[1].FallbackFrom != profA {
		t.Fatalf("the fallback target goes through the guard and the budget again: %v", p.log)
	}
	if p.index("settle:failed") > p.index("reserve:"+string(profB)) {
		t.Fatalf("the primary reservation settles before the target reserves: %v", p.log)
	}

	// The local provider named for unavailable serves; named only for another condition it does not.
	l := newRig(t, ModeExternal)
	l.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false)
	l.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondUnavailable}}
	o, err = l.execute(l.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Target != TargetLocal || len(o.Hops) != 1 || o.Hops[0].To != LocalHop {
		t.Fatalf("named local fallback: %+v", o)
	}
	w := newRig(t, ModeExternal)
	w.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false)
	w.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondTimeout}}
	o, err = w.execute(w.request(RoleController))
	mustKind(t, o, err, KindUnavailable)
	if w.count("local") != 0 {
		t.Fatalf("local was named for timeout only: %v", w.log)
	}

	// Rate limiting follows the same rule, and a budget refusal never takes a profile fallback.
	b := newRig(t, ModeExternal)
	b.profiles.fallbackTo = profB
	b.budgets.reserve = contracts.New(contracts.ErrResourceExhausted, "refused").WithDetails("reason", "budget_exhausted")
	o, err = b.execute(b.request(RoleController))
	mustKind(t, o, err, KindBudgetExhausted)
	if b.profiles.fallbackCalls != 0 {
		t.Fatal("an exhausted budget must never ask for a fallback")
	}
}

func TestNetworkLossMarksUncertainNotDuplicatedCommit(t *testing.T) {
	r := newRig(t, ModeExternal)
	r.profiles.fallbackTo = profB
	r.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondUnavailable, CondTimeout}}
	r.sendErr[profA] = callErr(pv.ProviderErrorKindConnectionLost, true)
	req := r.request(RoleController)
	req.CommitCapable = true
	o, err := r.execute(req)
	mustKind(t, o, err, KindUncertain)
	if r.count("send:") != 1 || r.count("fallback:") != 0 || r.count("local") != 0 || r.count("reserve:") != 1 {
		t.Fatalf("exactly one send, no retry, no fallback, no local re-run: %v", r.log)
	}
	if got := r.budgets.settled; len(got) != 1 || got[0].Outcome != pv.UsageRecordOutcomeUncertain || got[0].ErrorKind == nil || *got[0].ErrorKind != pv.ProviderErrorKindConnectionLost {
		t.Fatalf("the budget must settle as uncertain: %+v", got)
	}
	if !o.DataSent || contracts.CodeOf(err) != contracts.ErrUncertainOutcome || contracts.CodeOf(err).Retryable() {
		t.Fatalf("uncertain_outcome must be a non-retryable code and say data may have left: %+v %v", o, err)
	}

	// A foreign error from the sender is read as a lost connection after the send.
	f := newRig(t, ModeExternal)
	f.sendErr[profA] = errors.New("read tcp: connection reset")
	req = f.request(RoleController)
	req.CommitCapable = true
	o, err = f.execute(req)
	mustKind(t, o, err, KindUncertain)

	// A timeout after the send on a request that could commit is the same: no second send.
	tm := newRig(t, ModeExternal)
	tm.profiles.fallbackTo = profB
	tm.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondTimeout}}
	tm.sendErr[profA] = callErr(pv.ProviderErrorKindTimeout, true)
	req = tm.request(RoleController)
	req.CommitCapable = true
	o, err = tm.execute(req)
	mustKind(t, o, err, KindUncertain)
	if tm.count("send:") != 1 || tm.profiles.fallbackCalls != 0 || tm.count("local") != 0 || contracts.CodeOf(err).Retryable() {
		t.Fatalf("a timeout that may have reached the provider must not be retried: %v", tm.log)
	}

	// A request that cannot cause a commit is an ordinary unavailable and may fall back when named.
	n := newRig(t, ModeExternal)
	n.sendErr[profA] = callErr(pv.ProviderErrorKindConnectionLost, true)
	o, err = n.execute(n.request(RoleController))
	mustKind(t, o, err, KindUnavailable)
	if n.budgets.settled[0].Outcome != pv.UsageRecordOutcomeUncertain {
		t.Fatalf("it may still have been billed: %+v", n.budgets.settled)
	}
}

func TestUnsupportedModalitySelectsOtherQualifiedOrReportsUnsupported(t *testing.T) {
	capErr := reasonErr(contracts.ErrValidationFailed, providerreg.ReasonCapability)
	r := newRig(t, ModeExternal)
	r.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA, profB}}
	r.profiles.checkErr[profA] = capErr
	o, err := r.execute(r.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Profile != profB || r.count("send:"+string(profB)) != 1 || r.count("send:"+string(profA)) != 0 {
		t.Fatalf("the next qualified candidate must serve: %v %+v", r.log, o)
	}

	u := newRig(t, ModeExternal)
	u.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA, profB}}
	u.profiles.checkErr[profA], u.profiles.checkErr[profB] = capErr, capErr
	o, err = u.execute(u.request(RoleController))
	mustKind(t, o, err, KindUnsupportedModality)
	if u.count("guard:")+u.count("send:")+u.count("local") != 0 || o.DataSent {
		t.Fatalf("an unsupported request sends nothing: %v", u.log)
	}

	// The provider itself reporting an unsupported modality is the same row.
	s := newRig(t, ModeExternal)
	s.sendErr[profA] = callErr(pv.ProviderErrorKindUnsupportedModality, true)
	o, err = s.execute(s.request(RoleController))
	mustKind(t, o, err, KindUnsupportedModality)
}

// ---- the kill switch and the decision layer ---------------------------------------------------

func TestKillSwitchForcesLocal(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeKS)
	}{
		{"engaged", func(k *fakeKS) { k.engaged = true }},
		{"unreadable", func(k *fakeKS) { k.err = errors.New("store down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, ModeExternal)
			tc.set(r.ks)
			res, _ := r.router.Resolver().Resolve(context.Background(), testScope)
			if res.Mode != ModeLocal || !res.KillSwitch || res.Configured != ModeExternal || res.DecidedBy != DecidedByKillSwitch {
				t.Fatalf("the kill switch must force local over a configured external: %+v", res)
			}
			o, err := r.execute(r.request(RoleController))
			mustKind(t, o, err, KindServed)
			if o.Target != TargetLocal || o.Reason != DecidedByKillSwitch {
				t.Fatalf("%+v", o)
			}
			if r.count("check:")+r.count("guard:")+r.count("reserve:")+r.count("send:") != 0 {
				t.Fatalf("nothing external under the kill switch: %v", r.log)
			}
		})
	}

	// Engaged while a request is in flight: stopped before the call, served locally.
	m := newRig(t, ModeExternal)
	m.router.profiles = &killOnCheck{fakeProfiles: m.profiles, ks: m.ks}
	o, err := m.execute(m.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Target != TargetLocal || m.count("guard:")+m.count("reserve:")+m.count("send:") != 0 {
		t.Fatalf("an engaged switch stops a request before its call: %v %+v", m.log, o)
	}

	// The guard's own kill-switch denial is also served locally, whatever the policy names.
	g := newRig(t, ModeExternal)
	g.guard.deny = contracts.New(contracts.ErrPermissionDenied, "egress denied").WithDetails("rule", "kill_switch").WithDetails("check", "kill_switch")
	o, err = g.execute(g.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Target != TargetLocal || g.count("send:") != 0 {
		t.Fatalf("%+v", o)
	}
}

type killOnCheck struct {
	*fakeProfiles
	ks *fakeKS
}

func (k *killOnCheck) Check(ctx context.Context, s contracts.Scope, id pv.ProfileID, req providerreg.Requirement) (providerreg.Admission, error) {
	a, err := k.fakeProfiles.Check(ctx, s, id, req)
	k.ks.set(true)
	return a, err
}

func TestRouterRefusesDecisionScoringRoleForExternalProviders(t *testing.T) {
	if _, err := RoleDecisionScorer.External(); err == nil || contracts.CodeOf(err) != contracts.ErrPermissionDenied || detailReason(err, "") != "decision_scorer_local_only" {
		t.Fatalf("the decision scorer must have no external role: %v", err)
	}
	for _, role := range []Role{RoleReranker, RoleVAD, RoleEmbedder} {
		if _, err := role.External(); err == nil {
			t.Fatalf("%s must never leave", role)
		}
	}
	for _, role := range []Role{RoleController, RoleRealisation, RoleVision, RoleImageGeneration, RoleSTT, RoleTTS} {
		if _, err := role.External(); err != nil {
			t.Fatalf("%s may be external: %v", role, err)
		}
	}
	// A policy that lists a provider for it is refused.
	if err := (StaticPolicy{RoleDecisionScorer: {Profiles: []pv.ProfileID{profA}}}).Validate(); err == nil {
		t.Fatal("a policy may not route the decision scorer to a provider")
	}

	// Even with such a policy loaded and external mode on, the scorer never reaches the registry, the
	// guard, the budget or a provider; it is served by the local provider or not at all.
	for _, localOK := range []bool{true, false} {
		r := newRig(t, ModeExternal)
		r.policy[RoleDecisionScorer] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondUnavailable}}
		r.local[RoleDecisionScorer] = localOK
		o, err := r.execute(r.request(RoleDecisionScorer))
		if localOK {
			mustKind(t, o, err, KindServed)
			if o.Target != TargetLocal || o.Reason != "decision_scorer_local_only" {
				t.Fatalf("%+v", o)
			}
		} else {
			mustKind(t, o, err, KindUnavailable)
		}
		if r.count("check:")+r.count("guard:")+r.count("reserve:")+r.count("send:")+r.count("fallback:") != 0 || o.DataSent {
			t.Fatalf("the decision layer never reaches an external provider: %v", r.log)
		}
	}
}

// ---- hybrid, the section 13 table, the locality report ----------------------------------------

func TestHybridKeepsSensitiveWorkLocal(t *testing.T) {
	r := newRig(t, ModeHybrid)
	req := r.request(RoleController)
	req.Requirement.DataClass = pv.DataClassificationConfidential
	o, err := r.execute(req)
	mustKind(t, o, err, KindServed)
	if o.Target != TargetLocal || r.count("send:")+r.count("guard:") != 0 {
		t.Fatalf("hybrid keeps confidential work local: %v", r.log)
	}
	o, err = r.execute(r.request(RoleController))
	mustKind(t, o, err, KindServed)
	if o.Target != TargetExternal {
		t.Fatalf("internal data may go to the provider in hybrid: %+v", o)
	}
}

// TestSection13Table asserts each row of the failure and fallback table of additive plan section 13.
func TestSection13Table(t *testing.T) {
	type row struct {
		name      string
		setup     func(*rig, *Request)
		want      Kind
		sends     int
		fallbacks int
	}
	rows := []row{
		{"provider unavailable", func(r *rig, _ *Request) { r.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false) }, KindUnavailable, 1, 1},
		{"network lost during a request that could commit", func(r *rig, q *Request) {
			q.CommitCapable = true
			r.sendErr[profA] = callErr(pv.ProviderErrorKindConnectionLost, true)
		}, KindUncertain, 1, 0},
		{"provider timeout", func(r *rig, _ *Request) { r.sendErr[profA] = callErr(pv.ProviderErrorKindTimeout, true) }, KindTimeout, 1, 1},
		{"provider rate limit", func(r *rig, _ *Request) { r.sendErr[profA] = callErr(pv.ProviderErrorKindRateLimited, false) }, KindRateLimited, 1, 1},
		{"malformed tool call", func(r *rig, _ *Request) { r.sendErr[profA] = callErr(pv.ProviderErrorKindMalformedResponse, true) }, KindMalformedOutput, 1, 0},
		{"unsupported modality", func(r *rig, _ *Request) {
			r.profiles.checkErr[profA] = reasonErr(contracts.ErrValidationFailed, providerreg.ReasonCapability)
		}, KindUnsupportedModality, 0, 1},
		{"egress denied", func(r *rig, _ *Request) {
			r.guard.deny = contracts.New(contracts.ErrPermissionDenied, "x").WithDetails("rule", "provider_allowlist")
		}, KindEgressDenied, 0, 0},
		{"budget exhausted", func(r *rig, _ *Request) {
			r.budgets.reserve = contracts.New(contracts.ErrResourceExhausted, "x").WithDetails("reason", "budget_exhausted")
		}, KindBudgetExhausted, 0, 0},
		{"stale terms", func(r *rig, _ *Request) {
			r.profiles.checkErr[profA] = reasonErr(contracts.ErrQuarantined, providerreg.ReasonTermsChanged)
		}, KindStaleProfile, 0, 0},
		{"local fallback unavailable", func(r *rig, _ *Request) {
			r.sendErr[profA] = callErr(pv.ProviderErrorKindUnavailable, false)
			r.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondUnavailable}}
			r.local[RoleController] = false
			r.build()
		}, KindNoFallback, 1, 1},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, ModeExternal)
			req := r.request(RoleController)
			c.setup(r, &req)
			o, err := r.execute(req)
			mustKind(t, o, err, c.want)
			if got := r.count("send:"); got != c.sends {
				t.Fatalf("sends = %d, want %d: %v", got, c.sends, r.log)
			}
			if r.profiles.fallbackCalls != c.fallbacks {
				t.Fatalf("registry fallback asked %d times, want %d: %v", r.profiles.fallbackCalls, c.fallbacks, r.log)
			}
			if c.want != KindUncertain && !o.AIActionsDisabled {
				t.Fatalf("a controller that could not be served disables AI actions: %+v", o)
			}
			if o.Served() || len(o.Hops) > 1 {
				t.Fatalf("%+v", o)
			}
		})
	}
	// The malformed output carries no fallback unless named; the rate limit reports when to retry.
	r := newRig(t, ModeExternal)
	r.sendErr[profA] = &CallError{Kind: pv.ProviderErrorKindRateLimited, RetryAfter: 2 * time.Second}
	o, _ := r.execute(r.request(RoleController))
	if o.RetryAfter != 2*time.Second {
		t.Fatalf("retry-after lost: %+v", o)
	}
}

func TestFeatureLocalityReport(t *testing.T) {
	scopeReport := func(r *rig) Report {
		t.Helper()
		rep, err := r.router.LocalityReport(context.Background(), testScope)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	must := func(rep Report, feature string) Entry {
		t.Helper()
		e, ok := rep.Entry(feature)
		if !ok {
			t.Fatalf("no entry for %s", feature)
		}
		return e
	}

	// Local mode: models run local, vision has no local provider and says so.
	r := newRig(t, ModeLocal)
	rep := scopeReport(r)
	if e := must(rep, "controller"); e.Locality != LocalityLocal || !e.Available || e.Offline != OfflineWorks {
		t.Fatalf("%+v", e)
	}
	if e := must(rep, "vision"); e.Locality != LocalityUnavailable || e.Available {
		t.Fatalf("a feature with no local provider in local mode is unavailable, never local: %+v", e)
	}
	if _, ok := rep.Entry("provider_oauth_consent"); ok || !rep.RequiredLocalReady || rep.CoveringLocalGap {
		t.Fatalf("%+v", rep)
	}

	// External mode with a controller and vision route, no local vision, no named fallback.
	x := newRig(t, ModeExternal)
	x.policy[RoleVision] = RolePolicy{Profiles: []pv.ProfileID{profA}}
	rep = scopeReport(x)
	if e := must(rep, "controller"); e.Locality != LocalityRemote || !e.Available || e.Offline != OfflineUnavailable || !e.LocalUsable || len(e.Providers) != 1 {
		t.Fatalf("a remote controller is remote and unavailable offline unless a fallback is named: %+v", e)
	}
	if e := must(rep, "vision"); e.Locality != LocalityRemote || e.LocalUsable || e.Offline != OfflineUnavailable {
		t.Fatalf("%+v", e)
	}
	if e := must(rep, "router"); e.Locality != LocalityLocal || e.Reason != "no_external_route" {
		t.Fatalf("the decision scorer is always local: %+v", e)
	}
	if e := must(rep, "embedder"); e.Locality != LocalityLocal {
		t.Fatalf("%+v", e)
	}
	for _, f := range controlFeatures {
		if e := must(rep, f); e.Locality != LocalityLocal {
			t.Fatalf("section 12 control-plane feature %s must stay local: %+v", f, e)
		}
	}
	for _, f := range networkFeatures {
		if e := must(rep, f); e.Locality != LocalityOfflineUnavailable {
			t.Fatalf("%+v", e)
		}
	}

	// A named fallback shows as such; an unqualified provider makes the feature provider-dependent.
	x.policy[RoleController] = RolePolicy{Profiles: []pv.ProfileID{profA}, LocalFallback: []Condition{CondUnavailable}}
	if e := must(scopeReport(x), "controller"); e.Offline != OfflineLocalFallback {
		t.Fatalf("%+v", e)
	}
	x.profiles.lifecycle[profA] = pv.ProfileLifecycleQuarantined
	if e := must(scopeReport(x), "controller"); e.Locality != LocalityProviderDependent || e.Available || e.Reason != "provider_not_qualified" {
		t.Fatalf("%+v", e)
	}
	x.profiles.lifecycle[profA] = pv.ProfileLifecycleQualified
	x.budgets.states = nil
	if e := must(scopeReport(x), "controller"); e.Locality != LocalityProviderDependent || e.Reason != "no_budget" {
		t.Fatalf("a provider with no budget is not ready: %+v", e)
	}

	// Hybrid with a local controller depends on the provider per request.
	h := newRig(t, ModeHybrid)
	if e := must(scopeReport(h), "controller"); e.Locality != LocalityProviderDependent || !e.Available {
		t.Fatalf("%+v", e)
	}

	// External mode must not hide a local gap: the report says the local deployment is not ready.
	g := newRig(t, ModeExternal)
	g.local[RoleController] = false
	rep = scopeReport(g)
	if rep.RequiredLocalReady || !rep.CoveringLocalGap {
		t.Fatalf("external mode covering a failed local controller must be reported: %+v", rep)
	}
	if e := must(rep, "controller"); e.Locality != LocalityRemote || e.LocalUsable {
		t.Fatalf("%+v", e)
	}

	// The kill switch: the same feature is unavailable, not remote.
	k := newRig(t, ModeExternal)
	k.local[RoleController] = false
	k.ks.set(true)
	rep = scopeReport(k)
	if e := must(rep, "controller"); e.Locality != LocalityUnavailable || e.Available || e.Reason != "no_local_provider" || !rep.KillSwitch {
		t.Fatalf("%+v", e)
	}
}
