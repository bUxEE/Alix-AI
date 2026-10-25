package contracts

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// validScope returns a fully populated Scope. Each call returns fresh slices.
func validScope() Scope {
	return Scope{
		DeploymentID:     "dep_local",
		ApplicationID:    "app_crm",
		TenantID:         "tnt_acme",
		WorkspaceID:      "wsp_sales",
		PrincipalID:      "usr_42",
		Roles:            []string{"agent"},
		CapabilityScopes: []string{"tasks.create", "tasks.cancel"},
		SessionID:        "ses_01J0000000000000000000000A",
	}
}

func levelsOf(t *testing.T, err error, code ErrCode) []string {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v (%T) is not *contracts.Error", err, err)
	}
	if ce.Code != code {
		t.Fatalf("code = %q, want %q (%v)", ce.Code, code, err)
	}
	levels, ok := ce.Details["levels"].([]string)
	if !ok {
		t.Fatalf("Details[levels] = %#v", ce.Details["levels"])
	}
	for _, l := range levels {
		if !strings.Contains(ce.Message, l) {
			t.Errorf("message %q does not name level %q", ce.Message, l)
		}
	}
	return levels
}

func TestScopeValidateRequiresAllLevels(t *testing.T) {
	if err := validScope().Validate(); err != nil {
		t.Fatalf("fully populated scope must validate: %v", err)
	}

	cases := []struct {
		level  ScopeLevel
		mutate func(*Scope)
	}{
		{LevelDeployment, func(s *Scope) { s.DeploymentID = "" }},
		{LevelApplication, func(s *Scope) { s.ApplicationID = "" }},
		{LevelTenant, func(s *Scope) { s.TenantID = "" }},
		{LevelWorkspace, func(s *Scope) { s.WorkspaceID = "" }},
		{LevelPrincipal, func(s *Scope) { s.PrincipalID = "" }},
		{LevelRoles, func(s *Scope) { s.Roles = nil }},
		{LevelRoles, func(s *Scope) { s.Roles = []string{} }},
		{LevelRoles, func(s *Scope) { s.Roles = []string{"agent", ""} }},
		{LevelCapabilityScopes, func(s *Scope) { s.CapabilityScopes = nil }},
		{LevelCapabilityScopes, func(s *Scope) { s.CapabilityScopes = []string{} }},
		{LevelCapabilityScopes, func(s *Scope) { s.CapabilityScopes = []string{"tasks.create", "  "} }},
		{LevelSession, func(s *Scope) { s.SessionID = "" }},
	}
	for _, c := range cases {
		s := validScope()
		c.mutate(&s)
		err := s.Validate()
		if err == nil {
			t.Errorf("%s: scope with the level missing must not validate", c.level)
			continue
		}
		if got, want := levelsOf(t, err, ErrScopeMissing), []string{string(c.level)}; !cmp.Equal(got, want) {
			t.Errorf("%s: named levels %v, want %v", c.level, got, want)
		}
		if s.Permits("tasks.create") {
			t.Errorf("%s: an invalid scope must permit nothing", c.level)
		}
	}

	// Several missing levels are all named, in hierarchy order.
	s := validScope()
	s.TenantID, s.PrincipalID, s.SessionID = "", "", ""
	got := levelsOf(t, s.Validate(), ErrScopeMissing)
	if want := []string{"tenant", "principal", "session"}; !cmp.Equal(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}

	// scope_missing is not retryable: the caller must authenticate properly.
	if ErrScopeMissing.Retryable() {
		t.Error("scope_missing must not be retryable")
	}
}

func TestScopeNoImplicitDefaults(t *testing.T) {
	// The zero value is invalid at every level and nothing fills it in.
	var zero Scope
	if !zero.IsZero() {
		t.Fatal("zero Scope must report IsZero")
	}
	err := zero.Validate()
	want := []string{"deployment", "application", "tenant", "workspace", "principal", "roles", "capability_scopes", "session"}
	if got := levelsOf(t, err, ErrScopeMissing); !cmp.Equal(got, want) {
		t.Errorf("zero scope missing levels = %v, want %v", got, want)
	}
	if !zero.IsZero() || zero.TenantID != "" || zero.Roles != nil {
		t.Error("Validate must not fill defaults into the receiver")
	}
	if zero.Permits("tasks.create") || zero.Permits("") {
		t.Error("the zero Scope permits nothing")
	}

	// Whitespace-only is not a value (including Unicode spaces: NBSP, ideographic space).
	for _, blank := range []string{" ", "\t", "\n", " \xc2\xa0 ", "\xe3\x80\x80"} {
		s := validScope()
		s.TenantID = blank
		levelsOf(t, s.Validate(), ErrScopeMissing)
	}

	// No wildcards, anywhere: not as a tenant, not inside roles or capability scopes.
	wildcards := []struct {
		level  ScopeLevel
		mutate func(*Scope)
	}{
		{LevelDeployment, func(s *Scope) { s.DeploymentID = "*" }},
		{LevelApplication, func(s *Scope) { s.ApplicationID = "*" }},
		{LevelTenant, func(s *Scope) { s.TenantID = "*" }},
		{LevelTenant, func(s *Scope) { s.TenantID = "acme*" }},
		{LevelWorkspace, func(s *Scope) { s.WorkspaceID = "*" }},
		{LevelPrincipal, func(s *Scope) { s.PrincipalID = "*" }},
		{LevelRoles, func(s *Scope) { s.Roles = []string{"*"} }},
		{LevelCapabilityScopes, func(s *Scope) { s.CapabilityScopes = []string{"*"} }},
		{LevelCapabilityScopes, func(s *Scope) { s.CapabilityScopes = []string{"tasks.*"} }},
		{LevelSession, func(s *Scope) { s.SessionID = "*" }},
	}
	for _, c := range wildcards {
		s := validScope()
		c.mutate(&s)
		got := levelsOf(t, s.Validate(), ErrScopeViolation)
		if want := []string{string(c.level)}; !cmp.Equal(got, want) {
			t.Errorf("wildcard in %s: named %v, want %v", c.level, got, want)
		}
		if s.Permits("tasks.create") || s.Permits("*") {
			t.Errorf("wildcard in %s: scope must permit nothing", c.level)
		}
	}

	// Padding, control characters, invalid UTF-8 and over-long values are malformed.
	malformed := []string{" tenant", "tenant ", "ten ant", "ten\x00ant", "ten\nant", "bad\xffutf8", strings.Repeat("a", MaxScopeValueLen+1)}
	for _, v := range malformed {
		s := validScope()
		s.WorkspaceID = v
		levelsOf(t, s.Validate(), ErrScopeViolation)
	}
	s := validScope()
	s.WorkspaceID = strings.Repeat("a", MaxScopeValueLen)
	if err := s.Validate(); err != nil {
		t.Errorf("a value of exactly MaxScopeValueLen must validate: %v", err)
	}

	// A literal "default" is just an ID the host chose; the package never
	// invents it, and a missing tenant is never read as "default".
	s = validScope()
	s.TenantID = ""
	if s.Validate() == nil || s.TenantID != "" {
		t.Error("a missing tenant must stay missing")
	}

	// Missing takes precedence over malformed.
	s = validScope()
	s.TenantID, s.WorkspaceID = "*", ""
	got := levelsOf(t, s.Validate(), ErrScopeMissing)
	if want := []string{"workspace"}; !cmp.Equal(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}
}

func TestScopePermitsIsExactMatch(t *testing.T) {
	s := validScope()
	if !s.Permits("tasks.create") || !s.Permits("tasks.cancel") {
		t.Error("listed capabilities must be permitted")
	}
	for _, c := range []string{"", "tasks", "tasks.create.extra", "TASKS.CREATE", "tasks.*", "*"} {
		if s.Permits(c) {
			t.Errorf("%q must not be permitted", c)
		}
	}
}

func TestScopeCloneIsDeep(t *testing.T) {
	a := validScope()
	b := a.Clone()
	b.Roles[0] = "admin"
	b.CapabilityScopes[0] = "x"
	if a.Roles[0] != "agent" || a.CapabilityScopes[0] != "tasks.create" {
		t.Error("Clone must not share slices with the original")
	}
	if diff := cmp.Diff(validScope(), a); diff != "" {
		t.Errorf("original changed: %s", diff)
	}
}
