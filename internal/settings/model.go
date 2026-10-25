package settings

import (
	"context"
	"encoding/json"
	"time"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Level says whose settings a document holds (master plan 14.2: "authorized user,
// tenant and deployment settings").
type Level string

// The three levels, from the most general to the most specific.
const (
	// LevelDeployment is the application as a whole: every tenant and user of the
	// token's application. It is per application, never installation-wide (ADR
	// 0016: a token for application A can not read or write what application B
	// has). What the operator controls for the whole installation is configuration
	// (the settings section) or an operator-only API, not a setting.
	LevelDeployment Level = "deployment"
	// LevelTenant is the token's tenant of the token's application.
	LevelTenant Level = "tenant"
	// LevelUser is the token's principal in the token's workspace.
	LevelUser Level = "user"
)

// Levels lists the three levels, most general first, which is the order the
// effective settings are computed in.
func Levels() []Level { return []Level{LevelDeployment, LevelTenant, LevelUser} }

// Valid reports whether l is one of the three levels.
func (l Level) Valid() bool { return l == LevelDeployment || l == LevelTenant || l == LevelUser }

// SchemaID is the urn of the schema the documents of the level are validated
// against. It is stored with every document, so a later schema version can migrate
// what is stored instead of guessing at it.
func (l Level) SchemaID() string {
	switch l {
	case LevelDeployment:
		return "urn:alixai:schema:settings:deployment-settings:v1"
	case LevelTenant:
		return "urn:alixai:schema:settings:tenant-settings:v1"
	case LevelUser:
		return "urn:alixai:schema:settings:user-settings:v1"
	}
	return ""
}

// EffectiveSchemaID is the urn of the schema of the effective settings.
const EffectiveSchemaID = "urn:alixai:schema:settings:effective-settings:v1"

// Operation is what a caller does to a level.
type Operation string

// The two operations.
const (
	OpRead  Operation = "read"
	OpWrite Operation = "write"
)

// The capability scopes a context token must carry for the settings operations.
// They name platform operations, not application capabilities, and the host
// chooses which of its principals get them by putting them in their tokens (the
// same device applications.register and audit export use). A scope is an exact
// match: there are no wildcards, and a token that does not list one is refused.
const (
	CapabilityUserRead        = "settings.user.read"
	CapabilityUserWrite       = "settings.user.write"
	CapabilityTenantRead      = "settings.tenant.read"
	CapabilityTenantWrite     = "settings.tenant.write"
	CapabilityDeploymentRead  = "settings.deployment.read"
	CapabilityDeploymentWrite = "settings.deployment.write"
)

// RequiredCapability returns the capability scope a token needs for op on level,
// or "" for an unknown level or operation (which nothing permits).
func RequiredCapability(level Level, op Operation) string {
	switch {
	case level == LevelUser && op == OpRead:
		return CapabilityUserRead
	case level == LevelUser && op == OpWrite:
		return CapabilityUserWrite
	case level == LevelTenant && op == OpRead:
		return CapabilityTenantRead
	case level == LevelTenant && op == OpWrite:
		return CapabilityTenantWrite
	case level == LevelDeployment && op == OpRead:
		return CapabilityDeploymentRead
	case level == LevelDeployment && op == OpWrite:
		return CapabilityDeploymentWrite
	}
	return ""
}

// Record is one settings document as the API returns it.
type Record struct {
	Level Level `json:"level"`
	// Version is the revision: 0 when the level has no document yet (Settings is
	// then the empty document, and a first write expects 0), otherwise 1 for the
	// first write and one more for every change.
	Version int64 `json:"version"`
	// Schema is the urn of the schema Settings is an instance of.
	Schema string `json:"schema"`
	// UpdatedAt and UpdatedBy say when and by whom the document last changed;
	// both are absent while Version is 0.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	// Settings is the canonical JSON of the typed document.
	Settings json.RawMessage `json:"settings"`
}

// PutRequest replaces the document of one level.
type PutRequest struct {
	// ExpectedVersion is the Version the caller read: 0 to create the document.
	// The write succeeds only when it is still the stored version; otherwise it is
	// refused with conflict and nothing changes.
	ExpectedVersion int64
	// Settings is the whole new document. A member left out means inherit.
	Settings json.RawMessage
}

// Profile is one approved model and resource profile.
type Profile struct {
	ID string `json:"id"`
	// Default is true for the profile in force when no level states one.
	Default bool `json:"default"`
	// Selected is true for the profile the caller's effective settings select.
	Selected bool `json:"selected"`
}

// ProfileList is the approved profiles and the caller's choice among them.
type ProfileList struct {
	Profiles []Profile `json:"profiles"`
}

// CapabilityState is one capability the policy gateway exposes to the caller, with
// what settings do to it. Settings can only switch a capability off and add
// confirmation: Enabled is never true for something the gateway did not expose,
// and the platform's requirements are reported as they are.
type CapabilityState struct {
	ID string `json:"id"`
	// RiskLevel is the effective risk level the policy gateway assigns (0 to 3).
	RiskLevel int `json:"risk_level"`
	// Enabled is false when any level switched the capability off.
	Enabled bool `json:"enabled"`
	// DisabledBy lists the levels that switched it off, most general first.
	DisabledBy []Level `json:"disabled_by"`
	// RequiresConfirmation and RequiresStepUp are what a commit needs: the platform
	// floor and host policy, plus the extra confirmation settings asked for. They
	// can only be raised by settings, never lowered.
	RequiresConfirmation bool `json:"requires_confirmation"`
	RequiresStepUp       bool `json:"requires_step_up"`
	// ExtraConfirmation is true when RequiresConfirmation is true only because
	// settings asked for it.
	ExtraConfirmation bool `json:"extra_confirmation"`
	// Fields are the fields the caller may supply, in declaration order.
	Fields []string `json:"fields"`
}

// Access says which settings operations the caller's token permits, so a client
// can show or hide a control. It is derived from the token's capability scopes
// and decides nothing: every operation checks the token again.
type Access struct {
	User       LevelAccess `json:"user"`
	Tenant     LevelAccess `json:"tenant"`
	Deployment LevelAccess `json:"deployment"`
}

// LevelAccess is the read and write permission on one level.
type LevelAccess struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
}

// CapabilityView is the enabled capabilities of the caller.
type CapabilityView struct {
	Capabilities []CapabilityState `json:"capabilities"`
	// SettingsAccess is what the token permits on the settings themselves.
	SettingsAccess Access `json:"settings_access"`
}

// AuditCounts counts the findings of an integration audit by severity.
type AuditCounts struct {
	Critical   int `json:"critical"`
	Warnings   int `json:"warnings"`
	Info       int `json:"info"`
	Unverified int `json:"unverified"`
}

// Remediation statuses of an integration-audit finding.
const (
	// FindingOpen is a finding nobody has dealt with: a critical one must be fixed
	// (it can not be accepted) and a warning must be fixed or accepted.
	FindingOpen = "open"
	// FindingAccepted is a warning an authorised operator accepted.
	FindingAccepted = "accepted"
	// FindingInformational is a fact worth recording that needs no action.
	FindingInformational = "informational"
)

// AuditFinding is one finding of the latest integration audit with its
// remediation status.
type AuditFinding struct {
	Key         string `json:"key"`
	CheckID     string `json:"check_id"`
	Severity    string `json:"severity"`
	Capability  string `json:"capability,omitempty"`
	Path        string `json:"path,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	// Status is open, accepted or informational.
	Status string `json:"status"`
}

// AuditStatus is the latest integration audit of the caller's application:
// findings and where each stands. It is read only; nothing here accepts a
// warning, activates a package or changes what the application may do.
type AuditStatus struct {
	ApplicationID string `json:"application_id"`
	// State is active, quarantined, rejected, superseded or not_installed.
	State          string `json:"state"`
	PackageVersion string `json:"package_version,omitempty"`
	ManifestHash   string `json:"manifest_hash,omitempty"`
	// Audited is false when the installed package has never been audited.
	Audited bool   `json:"audited"`
	RunID   string `json:"run_id,omitempty"`
	// Outcome is blocked, needs_acceptance or clear, when Audited.
	Outcome  string         `json:"outcome,omitempty"`
	Counts   AuditCounts    `json:"counts"`
	Findings []AuditFinding `json:"findings"`
}

// ProviderRole is the health of one model role: its status and the stable
// degradation effect. It names no runtime, no address and no version: what runs
// behind a role is not something settings report or expose.
type ProviderRole struct {
	Role       string `json:"role"`
	Configured bool   `json:"configured"`
	// Status is ok, degraded or unavailable.
	Status string `json:"status"`
	// Effect is the stable degradation code when Status is not ok.
	Effect string `json:"effect,omitempty"`
}

// ProviderHealth is the health of the model roles.
type ProviderHealth struct {
	// Status is the worst status of any role.
	Status string         `json:"status"`
	Roles  []ProviderRole `json:"roles"`
}

// Package is the application package view the service validates settings against:
// the policy gateway's view of the active package plus the languages it supports.
// The composition root adapts the application registry to it; a package that is
// not active is refused before it gets here.
type Package interface {
	policy.ActivePackage
	// SupportedLanguages lists the languages the package supports; empty means it
	// does not restrict them.
	SupportedLanguages() []string
}

// PackageSource finds the active package of an application. It returns
// quarantined (or another typed error) for an application that has none.
type PackageSource interface {
	Package(ctx context.Context, applicationID string) (Package, error)
}

// AuditSource reads the latest integration audit of an application.
type AuditSource interface {
	IntegrationAudit(ctx context.Context, applicationID string) (AuditStatus, error)
}

// ProviderSource reads the health of the model roles.
type ProviderSource interface {
	ProviderHealth(ctx context.Context) (ProviderHealth, error)
}

// errNotConfigured is the answer of a read-out whose source was not wired.
func errNotConfigured(what string) error {
	return contracts.Newf(contracts.ErrUnavailable, "settings: %s is not available in this deployment", what)
}
