package contracts

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxScopeValueLen is the longest permitted scope identifier, role or
// capability scope, in characters. schemas/common/ids.schema.json mirrors it.
const MaxScopeValueLen = 128

// ScopeLevel names one level of the verified request context (master plan
// §10). The strings appear in scope_missing / scope_violation errors.
type ScopeLevel string

const (
	LevelDeployment       ScopeLevel = "deployment"
	LevelApplication      ScopeLevel = "application"
	LevelTenant           ScopeLevel = "tenant"
	LevelWorkspace        ScopeLevel = "workspace"
	LevelPrincipal        ScopeLevel = "principal"
	LevelRoles            ScopeLevel = "roles"
	LevelCapabilityScopes ScopeLevel = "capability_scopes"
	LevelSession          ScopeLevel = "session"
)

// Scope is the verified context every interactive or background request
// carries:
//
//	deployment -> application -> tenant -> workspace -> principal
//	  -> roles/scopes -> session
//
// Every level is required. There is no default tenant, no implicit workspace
// and no wildcard: the zero Scope is invalid, and nothing in this package ever
// fills a missing level in. A Scope is produced by the identity layer from a
// verified context token; code that receives one calls Validate before using
// it and treats any error as a denial.
//
// The JSON form is schemas/common/scope.schema.json.
type Scope struct {
	// DeploymentID identifies this AlixAI installation.
	DeploymentID string `json:"deployment_id"`
	// ApplicationID identifies the host application the request is bound to.
	ApplicationID string `json:"application_id"`
	// TenantID identifies the host application's tenant.
	TenantID string `json:"tenant_id"`
	// WorkspaceID identifies the workspace inside the tenant.
	WorkspaceID string `json:"workspace_id"`
	// PrincipalID identifies the authenticated human or service principal.
	PrincipalID string `json:"principal_id"`
	// Roles are the principal's roles, as asserted by the host application.
	Roles []string `json:"roles"`
	// CapabilityScopes are the capability IDs the context token permits.
	// Anything not listed is denied.
	CapabilityScopes []string `json:"capability_scopes"`
	// SessionID identifies the conversation session. A session cannot change
	// tenant or workspace without a new authenticated context.
	SessionID string `json:"session_id"`
}

// Validate reports whether every level of the scope is present and
// well-formed. It never modifies the Scope.
//
// A missing level (empty or whitespace-only value, empty Roles or
// CapabilityScopes, or a blank entry inside them) yields an *Error with code
// scope_missing whose message and Details["levels"] name every missing level
// in hierarchy order. A present but illegal value (a wildcard "*", leading or
// trailing whitespace, control characters, or more than MaxScopeValueLen
// characters) yields scope_violation naming the offending levels the same way.
// Missing levels take precedence over malformed ones.
func (s Scope) Validate() error {
	var missing, malformed []string
	check := func(level ScopeLevel, ok, present bool) {
		switch {
		case !present:
			missing = append(missing, string(level))
		case !ok:
			malformed = append(malformed, string(level))
		}
	}
	checkOne := func(level ScopeLevel, v string) {
		present, ok := scopeValueState(v)
		check(level, ok, present)
	}
	checkMany := func(level ScopeLevel, vs []string) {
		if len(vs) == 0 {
			missing = append(missing, string(level))
			return
		}
		allPresent, allOK := true, true
		for _, v := range vs {
			present, ok := scopeValueState(v)
			allPresent = allPresent && present
			allOK = allOK && ok
		}
		check(level, allOK, allPresent)
	}

	checkOne(LevelDeployment, s.DeploymentID)
	checkOne(LevelApplication, s.ApplicationID)
	checkOne(LevelTenant, s.TenantID)
	checkOne(LevelWorkspace, s.WorkspaceID)
	checkOne(LevelPrincipal, s.PrincipalID)
	checkMany(LevelRoles, s.Roles)
	checkMany(LevelCapabilityScopes, s.CapabilityScopes)
	checkOne(LevelSession, s.SessionID)

	switch {
	case len(missing) > 0:
		return New(ErrScopeMissing, "scope level missing: "+strings.Join(missing, ", ")).
			WithDetails("levels", missing)
	case len(malformed) > 0:
		return New(ErrScopeViolation, "scope level malformed (wildcards, padding, control characters and over-long values are not allowed): "+strings.Join(malformed, ", ")).
			WithDetails("levels", malformed)
	}
	return nil
}

// IsZero reports whether no level is set at all.
func (s Scope) IsZero() bool {
	return s.DeploymentID == "" && s.ApplicationID == "" && s.TenantID == "" &&
		s.WorkspaceID == "" && s.PrincipalID == "" && len(s.Roles) == 0 &&
		len(s.CapabilityScopes) == 0 && s.SessionID == ""
}

// Clone returns a deep copy, so the slices are not shared with the original.
func (s Scope) Clone() Scope {
	s.Roles = append([]string(nil), s.Roles...)
	s.CapabilityScopes = append([]string(nil), s.CapabilityScopes...)
	return s
}

// Permits reports whether the context token permits the given capability ID.
// It is an exact match: there are no wildcards, and an invalid Scope permits
// nothing.
func (s Scope) Permits(capabilityID string) bool {
	if s.Validate() != nil || capabilityID == "" {
		return false
	}
	for _, c := range s.CapabilityScopes {
		if c == capabilityID {
			return true
		}
	}
	return false
}

// scopeValueState classifies one scope value. present is false for the empty
// and whitespace-only strings (the level is missing); ok is false for a present
// value that breaks the identifier grammar.
func scopeValueState(v string) (present, ok bool) {
	if strings.TrimSpace(v) == "" {
		return false, false
	}
	if utf8.RuneCountInString(v) > MaxScopeValueLen || !utf8.ValidString(v) {
		return true, false
	}
	for _, r := range v {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return true, false
		}
	}
	return true, true
}
