package providerreg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// allConditions is a policy that names every condition a policy may name.
var allConditions = []providers.ProviderProfileFallbackOnItem{
	providers.ProviderProfileFallbackOnItemUnavailable, providers.ProviderProfileFallbackOnItemTimeout,
	providers.ProviderProfileFallbackOnItemRateLimited, providers.ProviderProfileFallbackOnItemUnsupportedModality,
}

// pair registers a qualified primary and a qualified fallback target and names the target on the
// primary for the given conditions.
func pair(g *rig, on []providers.ProviderProfileFallbackOnItem) (primary, target providers.ProfileID) {
	g.t.Helper()
	a := g.qualified(credRef(1))
	b := g.qualified(credRef(2), func(d *providers.ProviderProfile) { d.ProviderID = "fake-two" })
	to := b.ProfileID
	g.prof(g.reg.SetFallback(bg, g.scope, a.ProfileID, providers.ProviderProfileFallback{ProfileID: &to, On: on}))
	return a.ProfileID, b.ProfileID
}

func hasFallbackEvent(t *testing.T, g *rig, id providers.ProfileID) bool {
	t.Helper()
	return hasEvent(eventKinds(t, g, id), EventFallbackUsed)
}

func TestFallbackChainFollowedOnlyWhenPolicyNamesIt(t *testing.T) {
	g := newRig(t)
	a, b := pair(g, []providers.ProviderProfileFallbackOnItem{providers.ProviderProfileFallbackOnItemUnavailable, providers.ProviderProfileFallbackOnItemTimeout})

	t.Run("a named condition follows the fallback one hop and records it", func(t *testing.T) {
		g.clk.Advance(time.Minute)
		rec, adm, err := g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
		if err != nil || !adm.Valid() || adm.Profile.ProfileID != b {
			t.Fatalf("Fallback = %+v %+v %v", rec, adm.Profile.ProfileID, err)
		}
		if rec.From != a || rec.To != b || rec.Condition != FallbackOnUnavailable || !rec.At.Equal(g.clk.Now()) {
			t.Fatalf("record = %+v", rec)
		}
		var ev *Event
		evs := eventKinds(t, g, a)
		for i := range evs {
			if evs[i].Kind == EventFallbackUsed {
				ev = &evs[i]
			}
		}
		if ev == nil || ev.RelatedID != string(b) || ev.Reason != "unavailable" || ev.Actor != tAdmin || !ev.At.Equal(rec.At) {
			t.Fatalf("event = %+v", ev)
		}
	})

	t.Run("a condition the policy does not name selects nothing", func(t *testing.T) {
		for _, c := range []FallbackCondition{FallbackOnRateLimited, FallbackOnUnsupportedModality} {
			_, adm, err := g.reg.Fallback(bg, g.scope, a, c, req())
			wantReason(t, err, contracts.ErrPermissionDenied, ReasonFallbackNotNamed)
			if adm.Valid() {
				t.Fatalf("%s: an admission was returned", c)
			}
		}
	})

	t.Run("a profile with no fallback has none, and the target's own policy is not the primary's", func(t *testing.T) {
		_, _, err := g.reg.Fallback(bg, g.scope, b, FallbackOnUnavailable, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonFallbackNotNamed)
	})

	t.Run("a condition outside the closed set is refused", func(t *testing.T) {
		for _, c := range []FallbackCondition{"", "anything", "model_deprecated"} {
			_, _, err := g.reg.Fallback(bg, g.scope, a, c, req())
			wantReason(t, err, contracts.ErrPermissionDenied, ReasonFallbackConditionForbidden)
		}
	})

	t.Run("the target passes every gate for the same request", func(t *testing.T) {
		// The request needs image input: the target is text-only, so there is no hand-over.
		r := req()
		r.Input = append(r.Input, providers.ModalityImage)
		_, adm, err := g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, r)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonCapability)
		if adm.Valid() {
			t.Fatal("an admission was returned for an impossible request")
		}
		// Confidential data: the target allows public and internal only.
		r = req()
		r.DataClass = providers.DataClassificationConfidential
		_, _, err = g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, r)
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonDataClassDenied)
		// A scope the target does not permit: the hand-over is refused.
		g.prof(g.reg.SetPermittedScopes(bg, g.scope, b, nil))
		_, _, err = g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonScopeNotPermitted)
	})
}

func TestFallbackChainData(t *testing.T) {
	g := newRig(t)
	a, b := pair(g, allConditions)
	c := g.qualified(credRef(3), func(d *providers.ProviderProfile) { d.ProviderID = "fake-three" })

	// The chain follows the fallbacks the operator named.
	toC := c.ProfileID
	g.prof(g.reg.SetFallback(bg, g.scope, b, providers.ProviderProfileFallback{ProfileID: &toC, On: allConditions[:1]}))
	chain, err := g.reg.Chain(bg, g.scope, a)
	if err != nil || len(chain) != 3 || chain[0] != a || chain[1] != b || chain[2] != c.ProfileID {
		t.Fatalf("chain = %v, %v", chain, err)
	}

	// A profile is never its own fallback, a cycle is refused, and so is a target that is not
	// there, in another application, or a condition list that does not match the target.
	toA := a
	for name, tc := range map[string]struct {
		on providers.ProfileID
		fb providers.ProviderProfileFallback
	}{
		"self":          {a, providers.ProviderProfileFallback{ProfileID: &toA, On: allConditions[:1]}},
		"cycle":         {c.ProfileID, providers.ProviderProfileFallback{ProfileID: &toA, On: allConditions[:1]}},
		"unknown":       {a, providers.ProviderProfileFallback{ProfileID: ptr(providers.ProfileID("pprf_" + "00000000000000000000000000")), On: allConditions[:1]}},
		"target no on":  {a, providers.ProviderProfileFallback{ProfileID: &toC}},
		"on no target":  {a, providers.ProviderProfileFallback{On: allConditions[:1]}},
		"repeated on":   {a, providers.ProviderProfileFallback{ProfileID: &toC, On: []providers.ProviderProfileFallbackOnItem{allConditions[0], allConditions[0]}}},
		"unknown on":    {a, providers.ProviderProfileFallback{ProfileID: &toC, On: []providers.ProviderProfileFallbackOnItem{"invalid_credential"}}},
		"malformed tgt": {a, providers.ProviderProfileFallback{ProfileID: ptr(providers.ProfileID("x")), On: allConditions[:1]}},
	} {
		_, err := g.reg.SetFallback(bg, g.scope, tc.on, tc.fb)
		if ReasonOf(err) != ReasonFallbackInvalid {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	other := g.scopeOf(tOther, tTenant, tWsp)
	op := storetestOtherProfile(t, g, other)
	_, err = g.reg.SetFallback(bg, g.scope, a, providers.ProviderProfileFallback{ProfileID: &op, On: allConditions[:1]})
	wantReason(t, err, contracts.ErrValidationFailed, ReasonFallbackInvalid)

	// Registration also needs its fallback to exist in the application.
	d := g.draft()
	d.Fallback = providers.ProviderProfileFallback{ProfileID: &op, On: allConditions[:1]}
	if _, err := g.reg.Register(bg, g.scope, d); ReasonOf(err) != ReasonFallbackInvalid {
		t.Fatalf("Register with a foreign fallback: %v", err)
	}

	// Removing the fallback is allowed.
	got := g.prof(g.reg.SetFallback(bg, g.scope, a, providers.ProviderProfileFallback{}))
	if got.Fallback.ProfileID != nil || len(got.Fallback.On) != 0 {
		t.Fatalf("fallback = %+v", got.Fallback)
	}
	if _, _, err := g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req()); ReasonOf(err) != ReasonFallbackNotNamed {
		t.Fatalf("after removal: %v", err)
	}
}

// storetestOtherProfile registers a profile in another application and returns its id.
func storetestOtherProfile(t *testing.T, g *rig, scope contracts.Scope) providers.ProfileID {
	t.Helper()
	d := g.draft()
	d.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tOther}}
	p, err := g.reg.Register(bg, scope, d)
	if err != nil {
		t.Fatalf("Register in the other application: %v", err)
	}
	return p.ProfileID
}

func TestInvalidCredentialNeverSelectsAlternate(t *testing.T) {
	bad := []providers.CredentialRefStatus{
		providers.CredentialRefStatusInvalid, providers.CredentialRefStatusExpired, providers.CredentialRefStatusRevoked,
		providers.CredentialRefStatusReauthorizationRequired, providers.CredentialRefStatusQuarantined, providers.CredentialRefStatusPending,
	}
	for _, status := range bad {
		t.Run(string(status), func(t *testing.T) {
			g := newRig(t)
			a, b := pair(g, allConditions)
			pa := g.prof(g.reg.Get(bg, g.scope, a))
			g.creds.set(*pa.CredRef, status)

			// Using the primary is refused ...
			calls, err := g.route(g.scope, a, req())
			wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
			if calls != 0 {
				t.Fatal("send ran with an invalid credential")
			}
			// ... and so is a hand-over, whichever allowed condition the caller reports: a failure
			// reported as "unavailable" or "timeout" cannot launder a bad credential.
			for _, c := range []FallbackCondition{FallbackOnUnavailable, FallbackOnTimeout, FallbackOnRateLimited, FallbackOnUnsupportedModality} {
				_, adm, err := g.reg.Fallback(bg, g.scope, a, c, req())
				wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
				if adm.Valid() {
					t.Fatalf("%s: an alternate provider %s was selected", c, adm.Profile.ProfileID)
				}
			}
			if hasFallbackEvent(t, g, a) {
				t.Fatal("a fallback was recorded")
			}
			// A fallback target with an invalid credential is not used either.
			g.creds.set(*pa.CredRef, providers.CredentialRefStatusActive)
			pb := g.prof(g.reg.Get(bg, g.scope, b))
			g.creds.set(*pb.CredRef, status)
			_, _, err = g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
			wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
			if hasFallbackEvent(t, g, a) {
				t.Fatal("a fallback to an invalid credential was recorded")
			}
		})
	}

	t.Run("a credential service that fails is a refusal, never a pass", func(t *testing.T) {
		g := newRig(t)
		a, _ := pair(g, allConditions)
		g.creds.err = errors.New("credential service unreachable")
		calls, err := g.route(g.scope, a, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
		if calls != 0 {
			t.Fatal("send ran")
		}
		_, _, err = g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
	})

	t.Run("the conditions that never select an alternate are refused whatever the profile says", func(t *testing.T) {
		g := newRig(t)
		a, _ := pair(g, allConditions)
		for _, c := range []FallbackCondition{FallbackOnInvalidCredential, FallbackOnStaleTerms, FallbackOnEgressDenied, FallbackOnBudgetExhausted} {
			_, adm, err := g.reg.Fallback(bg, g.scope, a, c, req())
			wantReason(t, err, contracts.ErrPermissionDenied, ReasonFallbackConditionForbidden)
			if adm.Valid() {
				t.Fatalf("%s selected an alternate", c)
			}
		}
		if hasFallbackEvent(t, g, a) {
			t.Fatal("a forbidden fallback was recorded")
		}
	})

	t.Run("stale terms never hand over either", func(t *testing.T) {
		g := newRig(t)
		a, _ := pair(g, allConditions)
		g.clk.Set(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
		_, adm, err := g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
		wantReason(t, err, contracts.ErrQuarantined, ReasonStaleReview)
		if adm.Valid() || hasFallbackEvent(t, g, a) {
			t.Fatal("a stale profile handed over")
		}
	})

	t.Run("a quarantined or retired primary does not hand over", func(t *testing.T) {
		g := newRig(t)
		a, _ := pair(g, allConditions)
		g.prof(g.reg.Hold(bg, g.scope, a))
		_, _, err := g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
		wantReason(t, err, contracts.ErrQuarantined, ReasonOperatorHold)
		g.prof(g.reg.Retire(bg, g.scope, a))
		_, _, err = g.reg.Fallback(bg, g.scope, a, FallbackOnUnavailable, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonRetired)
	})
}

func TestRegistryRowsScoped(t *testing.T) {
	g := newRig(t)
	storetest.AssertScopedTables(t, g.db)

	a := g.qualified(credRef(1))
	otherScope := g.scopeOf(tOther, tTenant, tWsp)
	otherID := storetestOtherProfile(t, g, otherScope)

	t.Run("every row is application-level and carries its application", func(t *testing.T) {
		for _, table := range []string{"providerreg_profiles", "providerreg_events"} {
			var bad, n int
			if err := g.db.QueryRowContext(bg, `SELECT count(*), sum(application_id = '' OR tenant_id <> '' OR workspace_id <> '') FROM `+table).Scan(&n, &bad); err != nil {
				t.Fatal(err)
			}
			if n == 0 || bad != 0 {
				t.Fatalf("%s: %d rows, %d badly scoped", table, n, bad)
			}
		}
		var apps int
		if err := g.db.QueryRowContext(bg, `SELECT count(DISTINCT application_id) FROM providerreg_profiles`).Scan(&apps); err != nil || apps != 2 {
			t.Fatalf("applications = %d, %v", apps, err)
		}
	})

	t.Run("another application sees nothing of it: read, list, write and route", func(t *testing.T) {
		if _, err := g.reg.Get(bg, otherScope, a.ProfileID); ReasonOf(err) != ReasonProfileNotFound {
			t.Fatalf("Get across applications: %v", err)
		}
		list, err := g.reg.List(bg, otherScope)
		if err != nil || len(list) != 1 || list[0].ProfileID != otherID {
			t.Fatalf("List = %v, %v; want only the application's own profile", list, err)
		}
		mine, err := g.reg.List(bg, g.scope)
		if err != nil || len(mine) != 1 || mine[0].ProfileID != a.ProfileID {
			t.Fatalf("List = %v, %v", mine, err)
		}
		if _, err := g.reg.BindCredential(bg, otherScope, a.ProfileID, credRef(9)); ReasonOf(err) != ReasonProfileNotFound {
			t.Fatalf("BindCredential across applications: %v", err)
		}
		if _, err := g.reg.Retire(bg, otherScope, a.ProfileID); ReasonOf(err) != ReasonProfileNotFound {
			t.Fatalf("Retire across applications: %v", err)
		}
		if evs, err := g.reg.Events(bg, otherScope, a.ProfileID); err != nil || len(evs) != 0 {
			t.Fatalf("the other application read %d events of a profile that is not its own (%v)", len(evs), err)
		}
		if calls, err := g.route(otherScope, a.ProfileID, req()); ReasonOf(err) != ReasonProfileNotFound || calls != 0 {
			t.Fatalf("Route across applications: %d %v", calls, err)
		}
		if got := g.prof(g.reg.Get(bg, g.scope, a.ProfileID)); got.Lifecycle != providers.ProfileLifecycleQualified || got.CredRef == nil || *got.CredRef != credRef(1) {
			t.Fatal("a write from another application changed the profile")
		}
	})

	t.Run("every entry point refuses a missing scope", func(t *testing.T) {
		var none contracts.Scope
		partial := g.scope
		partial.WorkspaceID = ""
		for _, s := range []contracts.Scope{none, partial} {
			checks := map[string]error{}
			_, checks["Register"] = g.reg.Register(bg, s, g.draft())
			_, checks["Get"] = g.reg.Get(bg, s, a.ProfileID)
			_, checks["List"] = g.reg.List(bg, s)
			_, checks["Events"] = g.reg.Events(bg, s, a.ProfileID)
			_, checks["Check"] = g.reg.Check(bg, s, a.ProfileID, req())
			_, checks["Bind"] = g.reg.BindCredential(bg, s, a.ProfileID, credRef(1))
			_, checks["Ack"] = g.reg.AcknowledgeBillingNotice(bg, s, a.ProfileID, Acknowledgement{})
			_, checks["Qualify"] = g.reg.Qualify(bg, s, a.ProfileID)
			_, checks["Pin"] = g.reg.PinModelVersion(bg, s, a.ProfileID, "v9")
			_, checks["Revalidate"] = g.reg.Revalidate(bg, s)
			_, checks["SetFallback"] = g.reg.SetFallback(bg, s, a.ProfileID, providers.ProviderProfileFallback{})
			_, checks["SetScopes"] = g.reg.SetPermittedScopes(bg, s, a.ProfileID, nil)
			_, _, checks["Fallback"] = g.reg.Fallback(bg, s, a.ProfileID, FallbackOnUnavailable, req())
			checks["Route"] = g.reg.Route(bg, s, a.ProfileID, req(), func(_ context.Context, _ Admission) error { return nil })
			for name, err := range checks {
				if c := contracts.CodeOf(err); c != contracts.ErrScopeMissing {
					t.Errorf("%s with an incomplete scope: code = %s (%v), want scope_missing", name, c, err)
				}
			}
		}
	})

	t.Run("the registry insists on its collaborators", func(t *testing.T) {
		if _, err := New(nil, Config{Credentials: g.creds}); err == nil {
			t.Fatal("New accepted no store")
		}
		if _, err := New(g.db, Config{}); err == nil {
			t.Fatal("New accepted no credential status source: it could not tell a valid credential from an invalid one")
		}
	})
}
