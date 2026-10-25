package identity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-025: a session cannot change tenant or workspace without a new
// authenticated token, and a new token for another tenant is not a way to
// continue the old session.
func TestTenantSwitchRequiresNewToken(t *testing.T) {
	env := identitytest.NewEnv(t)

	orig := env.Issuer.Token()
	v1, err := env.Verifier.Verify(bg, orig)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Editing the tenant claim under the old signature is not a new token.
	for name, edit := range map[string]func(map[string]any){
		"tenant":    func(m map[string]any) { m[identity.ClaimTenant] = "ten_other" },
		"workspace": func(m map[string]any) { m[identity.ClaimWorkspace] = "wsp_other" },
		"principal": func(m map[string]any) { m[identity.ClaimSubject] = "usr_bob" },
	} {
		tampered := withPayload(t, env.Issuer.Token(), edit)
		_, err := env.Verifier.Verify(bg, tampered)
		if err == nil {
			t.Fatalf("%s switched by editing the claim", name)
		}
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadSignature)
	}

	// 2. A properly signed token for another tenant (or workspace) verifies as
	// itself, but it is not the same context as the session it names.
	cases := []struct {
		name   string
		mod    func(*identitytest.Draft)
		levels []string
	}{
		{"tenant", func(d *identitytest.Draft) { d.Set(identity.ClaimTenant, "ten_other") }, []string{"tenant"}},
		{"workspace", func(d *identitytest.Draft) { d.Set(identity.ClaimWorkspace, "wsp_other") }, []string{"workspace"}},
		{"both", func(d *identitytest.Draft) {
			d.Set(identity.ClaimTenant, "ten_other").Set(identity.ClaimWorkspace, "wsp_other")
		}, []string{"tenant", "workspace"}},
		{"principal", func(d *identitytest.Draft) { d.Set(identity.ClaimSubject, "usr_bob") }, []string{"principal"}},
		{"session", func(d *identitytest.Draft) { d.Set(identity.ClaimSession, "ses_other") }, []string{"session"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v2, err := env.Verifier.Verify(bg, env.Issuer.Token(c.mod))
			if err != nil {
				t.Fatalf("a validly signed token for another context was rejected outright: %v", err)
			}
			err = identity.CheckContinuity(v1.Scope, v2.Scope)
			wantScopeLevels(t, err, contracts.ErrScopeViolation, c.levels...)
		})
	}

	// 3. A refresh for the same context passes, even with different roles and
	// capability scopes and a later expiry: that is what a new token is for.
	refreshed, err := env.Verifier.Verify(bg, env.Issuer.Token(func(d *identitytest.Draft) {
		d.Set(identity.ClaimRoles, []string{"member", "reviewer"}).
			Set(identity.ClaimCapabilityScopes, []string{"reservations.read"}).
			Lifetime(10 * minute)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.CheckContinuity(v1.Scope, refreshed.Scope); err != nil {
		t.Fatalf("a same-context refresh was refused: %v", err)
	}
	if refreshed.Scope.Permits("reservations.create") {
		t.Fatal("the refreshed token's narrower capability scopes were not honoured")
	}

	// 4. A different deployment is a different context too, and an invalid
	// scope on either side is reported as such.
	other := v1.Scope.Clone()
	other.DeploymentID = "dep_other"
	wantScopeLevels(t, identity.CheckContinuity(v1.Scope, other), contracts.ErrScopeViolation, "deployment")
	if err := identity.CheckContinuity(contracts.Scope{}, v1.Scope); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Fatalf("empty bound scope: err = %v, want scope_missing", err)
	}
	if err := identity.CheckContinuity(v1.Scope, contracts.Scope{}); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Fatalf("empty presented scope: err = %v, want scope_missing", err)
	}
}

// NewScope is the single constructor of a Scope from asserted values, shared
// with the local IPC transport.
func TestNewScope(t *testing.T) {
	good := identity.ContextClaims{
		ApplicationID: "app_a", TenantID: "ten_a", WorkspaceID: "wsp_a", PrincipalID: "usr_a",
		Roles: []string{"member"}, CapabilityScopes: []string{"a.read"}, SessionID: "ses_a",
	}
	s, err := identity.NewScope("dep_main", good)
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.Scope{
		DeploymentID: "dep_main", ApplicationID: "app_a", TenantID: "ten_a", WorkspaceID: "wsp_a",
		PrincipalID: "usr_a", Roles: []string{"member"}, CapabilityScopes: []string{"a.read"}, SessionID: "ses_a",
	}
	if diff := cmp.Diff(want, s); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}

	t.Run("no deployment", func(t *testing.T) {
		if _, err := identity.NewScope("", good); !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Fatalf("err = %v, want scope_missing", err)
		}
	})
	t.Run("zero claims", func(t *testing.T) {
		if _, err := identity.NewScope("dep_main", identity.ContextClaims{}); !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Fatalf("err = %v, want scope_missing", err)
		}
	})
	t.Run("wildcard", func(t *testing.T) {
		bad := good
		bad.TenantID = "*"
		if _, err := identity.NewScope("dep_main", bad); !contracts.HasCode(err, contracts.ErrScopeViolation) {
			t.Fatalf("err = %v, want scope_violation", err)
		}
	})
	t.Run("the input slices are not shared", func(t *testing.T) {
		in := good
		in.Roles = []string{"member"}
		out, err := identity.NewScope("dep_main", in)
		if err != nil {
			t.Fatal(err)
		}
		in.Roles[0] = "admin"
		if out.Roles[0] != "member" {
			t.Fatal("NewScope aliased the caller's roles")
		}
	})
}

func TestContextCarriesOnlyValidScopes(t *testing.T) {
	scope := identitytest.ExpectedScope()

	if _, ok := identity.ScopeFromContext(context.Background()); ok {
		t.Fatal("an empty context has a scope")
	}
	if _, err := identity.RequireScope(context.Background()); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Fatalf("RequireScope on an empty context = %v, want scope_missing", err)
	}

	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	got, err := identity.RequireScope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(scope, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}

	// The scope handed out is a copy.
	got.Roles[0] = "root"
	again, _ := identity.ScopeFromContext(ctx)
	if again.Roles[0] != scope.Roles[0] {
		t.Fatal("mutating a returned scope changed the context's")
	}

	// An invalid scope never enters a context.
	for name, bad := range map[string]contracts.Scope{
		"zero":     {},
		"wildcard": func() contracts.Scope { s := scope.Clone(); s.WorkspaceID = "*"; return s }(),
		"no roles": func() contracts.Scope { s := scope.Clone(); s.Roles = nil; return s }(),
	} {
		if _, err := identity.WithScope(context.Background(), bad); err == nil {
			t.Fatalf("%s scope was stored", name)
		}
	}
	if _, err := identity.WithVerified(context.Background(), nil); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Fatalf("WithVerified(nil) = %v, want scope_missing", err)
	}
}

func TestConfigValidation(t *testing.T) {
	ok := identity.DefaultConfig()
	ok.DeploymentID = "dep_main"
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := identity.DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults (no deployment id yet) must load: %v", err)
	}
	cases := map[string]func(*identity.Config){
		"wildcard deployment": func(c *identity.Config) { c.DeploymentID = "*" },
		"padded deployment":   func(c *identity.Config) { c.DeploymentID = " dep" },
		"long deployment":     func(c *identity.Config) { c.DeploymentID = strings.Repeat("d", 129) },
		"audience space":      func(c *identity.Config) { c.Audience = "a b" },
		"lifetime zero":       func(c *identity.Config) { c.MaxTokenLifetime = 0 },
		"lifetime above 15m":  func(c *identity.Config) { c.MaxTokenLifetime = identity.MaxLifetime + 1 },
		"lifetime negative":   func(c *identity.Config) { c.MaxTokenLifetime = -1 },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			c := ok
			mod(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if lower := ok; true {
		lower.MaxTokenLifetime = 2 * minute
		if err := lower.Validate(); err != nil {
			t.Fatalf("a lower lifetime is the point of the option: %v", err)
		}
		if opts := lower.Options(); opts.MaxLifetime != 2*minute || opts.DeploymentID != "dep_main" {
			t.Fatalf("Options() = %+v", opts)
		}
	}
}
