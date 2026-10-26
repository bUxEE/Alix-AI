package modes

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestExternalNoRouteUsesLocalOrLlmNotConfigured(t *testing.T) {
	t.Run("local when usable", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		r.policy = StaticPolicy{}
		r.build()
		o, err := r.execute(r.request(RoleController))
		mustKind(t, o, err, KindServed)
		if o.Target != TargetLocal || o.Reason != "no_external_route" {
			t.Fatalf("got %+v", o)
		}
	})
	t.Run("llm_not_configured when local unusable", func(t *testing.T) {
		r := newRig(t, ModeExternal)
		r.policy = StaticPolicy{}
		r.local = fakeLocal{}
		r.build()
		o, err := r.execute(r.request(RoleController))
		mustKind(t, o, err, KindUnavailable)
		if o.Reason != "llm_not_configured" {
			t.Fatalf("got %+v", o)
		}
	})
}

type stubBindings struct {
	b     providerreg.PrincipalBinding
	found bool
}

func (s stubBindings) PrincipalBindingFor(context.Context, contracts.Scope, pv.PrincipalLlmRole) (providerreg.PrincipalBinding, bool, error) {
	return s.b, s.found, nil
}

type stubCreds struct {
	ref pv.CredentialRefID
	ok  bool
}

func (s stubCreds) ActivePrincipalCred(context.Context, contracts.Scope, pv.ProviderID) (pv.CredentialRefID, bool, error) {
	return s.ref, s.ok, nil
}

type countingProfiles struct {
	inner Profiles
	n     int
}

func (c *countingProfiles) Get(ctx context.Context, scope contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error) {
	return c.inner.Get(ctx, scope, id)
}
func (c *countingProfiles) Check(ctx context.Context, scope contracts.Scope, id pv.ProfileID, req providerreg.Requirement) (providerreg.Admission, error) {
	c.n++
	return c.inner.Check(ctx, scope, id, req)
}
func (c *countingProfiles) Fallback(ctx context.Context, scope contracts.Scope, from pv.ProfileID, cond providerreg.FallbackCondition, req providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return c.inner.Fallback(ctx, scope, from, cond, req)
}

func TestPrincipalProfilesDeniedCredentialEndsSelection(t *testing.T) {
	r := newRig(t, ModeExternal)
	bound := providerreg.PrincipalBinding{
		Role: pv.PrincipalLlmRoleConversationalController, ProfileID: profA,
		ProviderID: "openai", ModelID: "gpt-4o-mini",
	}
	inner := &countingProfiles{inner: r.profiles}
	pp := PrincipalProfiles{
		Inner:    inner,
		Registry: &providerreg.Registry{}, // unused when credential is missing
		Bindings: stubBindings{b: bound, found: true},
		Creds:    stubCreds{ok: false},
	}
	_, err := pp.Check(context.Background(), testScope, profA, providerreg.Requirement{Role: pv.RoleConversationalController})
	if err == nil || providerreg.ReasonOf(err) != providerreg.ReasonCredentialMissing {
		t.Fatalf("want credential_missing, got %v", err)
	}
	if inner.n != 0 {
		t.Fatalf("inner Check was called %d times; denied cred must end selection", inner.n)
	}
}

func TestPrincipalProfilesNeverUsesAnotherBinding(t *testing.T) {
	r := newRig(t, ModeExternal)
	inner := &countingProfiles{inner: r.profiles}
	// No binding for this principal → only Inner.Check (deployment default), never a foreign cred.
	pp := PrincipalProfiles{
		Inner: inner, Registry: &providerreg.Registry{},
		Bindings: stubBindings{found: false},
		Creds:    stubCreds{ok: true, ref: "cred_other"},
	}
	if _, err := pp.Check(context.Background(), testScope, profA, providerreg.Requirement{Role: pv.RoleConversationalController}); err != nil {
		t.Fatal(err)
	}
	if inner.n != 1 {
		t.Fatalf("inner checks = %d, want 1", inner.n)
	}
}
