package audit

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ChainKey names one hash chain. TenantID is empty for an application chain.
type ChainKey struct {
	ApplicationID string `json:"application_id"`
	TenantID      string `json:"tenant_id"`
}

// IsApplication reports whether k is an application chain (no tenant).
func (k ChainKey) IsApplication() bool { return k.TenantID == "" }

func (k ChainKey) String() string {
	if k.IsApplication() {
		return k.ApplicationID + "/-"
	}
	return k.ApplicationID + "/" + k.TenantID
}

// validate checks a chain key given by an operator (CheckpointAll, VerifyChain).
func (k ChainKey) validate() error {
	if k.IsApplication() {
		return checkApplicationID(k.ApplicationID, true)
	}
	if k.ApplicationID == trust.PlatformApplicationID {
		return contracts.New(contracts.ErrScopeViolation, "audit: the platform application has no tenant chains")
	}
	if err := checkID(contracts.LevelApplication, k.ApplicationID); err != nil {
		return err
	}
	return checkID(contracts.LevelTenant, k.TenantID)
}

type chainKind uint8

const (
	kindInvalid chainKind = iota
	kindTenant
	kindApplication
)

// Locator says where an event goes: which chain, which workspace, and on whose
// behalf. It can only be built by ForScope, ForIDs, ForApplication and
// Platform, so no caller can name a chain level it was not given: the zero
// Locator is refused by every append (there is no default chain).
type Locator struct {
	kind        chainKind
	application string
	tenant      string
	workspace   string
	principal   string
	session     string
}

// Chain returns the chain the locator addresses.
func (l Locator) Chain() ChainKey { return ChainKey{ApplicationID: l.application, TenantID: l.tenant} }

// Level returns the chain level the locator addresses, or zero for an invalid
// locator.
func (l Locator) Level() ChainLevels {
	switch l.kind {
	case kindTenant:
		return LevelTenant
	case kindApplication:
		return LevelApplication
	}
	return 0
}

// ForScope places an event in the tenant chain of a verified scope, in the
// scope's workspace, attributed to its principal and session. The scope must be
// valid in every level (contracts.Scope.Validate); the error is that
// validation's, scope_missing or scope_violation.
func ForScope(scope contracts.Scope) (Locator, error) {
	if err := scope.Validate(); err != nil {
		return Locator{}, err
	}
	return ForIDs(scope.ApplicationID, scope.TenantID, scope.WorkspaceID, scope.PrincipalID, scope.SessionID)
}

// ForIDs places an event in the tenant chain of (application, tenant), in
// workspace, attributed to principal and session. Application, tenant and
// workspace are required and must be well-formed identifiers (no wildcard, no
// padding, no control characters); principal and session may be empty. It is
// for records that carry their own subject, such as a policy.Record, and fails
// for anything missing so that the caller can decide where such an event goes
// (see RecordSink).
func ForIDs(application, tenant, workspace, principal, session string) (Locator, error) {
	for _, c := range []struct {
		level contracts.ScopeLevel
		v     string
	}{
		{contracts.LevelApplication, application},
		{contracts.LevelTenant, tenant},
		{contracts.LevelWorkspace, workspace},
	} {
		if err := checkID(c.level, c.v); err != nil {
			return Locator{}, err
		}
	}
	if application == trust.PlatformApplicationID {
		return Locator{}, contracts.New(contracts.ErrScopeViolation, "audit: the platform application has no tenant chains")
	}
	for _, c := range []struct {
		level contracts.ScopeLevel
		v     string
	}{
		{contracts.LevelPrincipal, principal},
		{contracts.LevelSession, session},
	} {
		if c.v == "" {
			continue
		}
		if err := checkID(c.level, c.v); err != nil {
			return Locator{}, err
		}
	}
	return Locator{kind: kindTenant, application: application, tenant: tenant, workspace: workspace, principal: principal, session: session}, nil
}

var applicationIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func checkApplicationID(id string, allowPlatform bool) error {
	if id == "" {
		return contracts.New(contracts.ErrScopeMissing, "audit: application id is missing")
	}
	if !applicationIDRE.MatchString(id) {
		return contracts.New(contracts.ErrScopeViolation, "audit: application id is malformed")
	}
	if id == trust.PlatformApplicationID && !allowPlatform {
		return contracts.New(contracts.ErrScopeViolation, "audit: the platform application is not a host application")
	}
	return nil
}

// ForApplication places an event in the application chain of a host
// application (package lifecycle, integration-audit runs), attributed to
// principal (an operator id; empty means the system). The application id must
// match the application id grammar. The platform application is Platform.
func ForApplication(application, principal string) (Locator, error) {
	if err := checkApplicationID(application, false); err != nil {
		return Locator{}, err
	}
	if principal != "" {
		if err := checkID(contracts.LevelPrincipal, principal); err != nil {
			return Locator{}, err
		}
	}
	return Locator{kind: kindApplication, application: application, principal: principal}, nil
}

// Platform places an event in the platform's own chain: events that belong to
// no application and no tenant, or whose subject could not be verified.
func Platform() Locator {
	return Locator{kind: kindApplication, application: trust.PlatformApplicationID}
}

// checkID applies the contracts identifier rules to one value: present, valid
// UTF-8, at most contracts.MaxScopeValueLen characters, no wildcard, no
// whitespace, no control characters.
func checkID(level contracts.ScopeLevel, v string) error {
	if strings.TrimSpace(v) == "" {
		return contracts.Newf(contracts.ErrScopeMissing, "audit: scope level missing: %s", level)
	}
	bad := !utf8.ValidString(v) || utf8.RuneCountInString(v) > contracts.MaxScopeValueLen
	if !bad {
		for _, r := range v {
			if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
				bad = true
				break
			}
		}
	}
	if bad {
		return contracts.Newf(contracts.ErrScopeViolation, "audit: scope level malformed: %s", level)
	}
	return nil
}
