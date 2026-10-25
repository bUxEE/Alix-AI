package policy

import (
	"encoding/json"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// RecordFormat is the value of Record.Format of a decision record: the full
// record of one decision.
const RecordFormat = "alixai.policy-decision/v1"

// ExposureFormat is the value of Record.Format of an exposure record: the
// compact record Gateway.Expose hands to the sink in place of one decision
// record per declared capability. Both formats are kinds of the one schema,
// schemas/policy/decision-record.schema.json.
const ExposureFormat = "alixai.policy-exposure/v1"

// Bounds of an exposure record. An audit event is a reference to what happened,
// not a copy of it, and the audit log refuses event data above 32 KiB
// (audit.DefaultMaxDataBytes), so Expose splits a catalogue that would not fit
// into several records (Part, Parts) instead of writing one the log would
// refuse. MaxExposureOutcomes is the capability limit of an application package
// (schemas/application-package/manifest.schema.json); MaxExposureRecordBytes
// leaves a quarter of the audit limit as headroom for canonical re-encoding.
const (
	MaxExposureOutcomes    = 256
	MaxExposureRecordBytes = 24 << 10
)

// RecordIDPrefix is the prefix of Record.DecisionID. It fits the typedId
// grammar of schemas/common/ids (2 to 8 lower-case letters).
const RecordIDPrefix = "pdec"

// Phase says where in a turn a decision is made. Policy runs before a tool is
// exposed to the model and again at prepare and at commit (architecture
// overview, section 5).
type Phase string

// Phases.
const (
	// PhaseExpose decides whether a capability may be shown to the model. No
	// field values exist yet.
	PhaseExpose Phase = "expose"
	// PhasePrepare decides whether a prepared action may be staged, with the
	// proposed field values.
	PhasePrepare Phase = "prepare"
	// PhaseCommit decides whether a prepared action may be committed, with the
	// field values and the confirmation evidence.
	PhaseCommit Phase = "commit"
)

// Valid reports whether p is one of the three phases.
func (p Phase) Valid() bool { return p == PhaseExpose || p == PhasePrepare || p == PhaseCommit }

// Outcome is allow or deny.
type Outcome string

// Outcomes. The zero Outcome is neither: a record without an outcome is not an
// allow.
const (
	OutcomeAllow Outcome = "allow"
	OutcomeDeny  Outcome = "deny"
)

// CheckName names one step of the decision order.
type CheckName string

// Check names, in evaluation order (audit_sink last, when a sink is installed).
const (
	CheckRequest     CheckName = "request"
	CheckScope       CheckName = "scope"
	CheckPackage     CheckName = "package_active"
	CheckApplication CheckName = "application_match"
	CheckTenant      CheckName = "tenant_workspace_match"
	CheckDeclared    CheckName = "capability_declared"
	CheckAudience    CheckName = "audience"
	CheckToken       CheckName = "token_scope"
	CheckRole        CheckName = "role_grant"
	CheckRules       CheckName = "host_rules"
	CheckFields      CheckName = "field_visibility"
	CheckEvidence    CheckName = "evidence"
	CheckSink        CheckName = "audit_sink"
)

// Check is the result of one step. Reason is a fixed machine-readable code,
// never text taken from the request. RuleID names the policy or rule that
// produced the outcome when one did.
type Check struct {
	Name   CheckName         `json:"name"`
	Passed bool              `json:"passed"`
	Code   contracts.ErrCode `json:"code,omitempty"`
	Reason string            `json:"reason"`
	RuleID string            `json:"rule_id,omitempty"`
}

// Subject is the verified context a decision was made for. Empty strings mean
// the level was absent from the request.
type Subject struct {
	ApplicationID    string   `json:"application_id"`
	TenantID         string   `json:"tenant_id"`
	WorkspaceID      string   `json:"workspace_id"`
	PrincipalID      string   `json:"principal_id"`
	SessionID        string   `json:"session_id"`
	Roles            []string `json:"roles"`
	CapabilityScopes []string `json:"capability_scopes"`
}

// PackageRef identifies the package a decision was made against.
type PackageRef struct {
	ApplicationID string `json:"application_id"`
	Version       string `json:"version"`
	Status        string `json:"status"`
}

// RiskRaise is one thing that lifted the effective level above the declared one.
type RiskRaise struct {
	// Source is "platform_floor", "risk_override" or "rule".
	Source string `json:"source"`
	// ID is the policy ID (risk_override) or rule ID (rule).
	ID    string              `json:"id,omitempty"`
	Level contracts.RiskLevel `json:"level"`
}

// Risk sources.
const (
	RaiseFloor    = "platform_floor"
	RaiseOverride = "risk_override"
	RaiseRule     = "rule"
)

// RiskRecord is the risk calculation of master plan 11.4.
type RiskRecord struct {
	Declared      contracts.RiskLevel `json:"declared"`
	PlatformFloor contracts.RiskLevel `json:"platform_floor"`
	HostMinimum   contracts.RiskLevel `json:"host_minimum"`
	Effective     contracts.RiskLevel `json:"effective"`
	// EffectClass is the declared effect class, or "unknown".
	EffectClass string      `json:"effect_class"`
	RaisedBy    []RiskRaise `json:"raised_by"`
}

// Requirements is what the effective level obliges the caller to obtain before
// a commit: an explicit confirmation, and for level 3 a step-up through the
// host application's own authenticated UI.
type Requirements struct {
	Confirmation bool `json:"confirmation"`
	StepUp       bool `json:"step_up"`
}

// Withheld reasons.
const (
	WithheldNotProjected = "not_projected"
	WithheldSensitive    = "sensitive_until_needed"
	WithheldRule         = "rule"
)

// WithheldField is a declared field the model may not see, and why.
type WithheldField struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	RuleID string `json:"rule_id,omitempty"`
}

// RuleApplied is a rule whose condition held and what it did.
type RuleApplied struct {
	RuleID  string   `json:"rule_id"`
	Effects []string `json:"effects"`
}

// Record is what the audit log stores for one policy decision
// (schemas/policy/decision-record.schema.json). It never holds a field value,
// a credential or text from the user.
//
// A Record is one of two kinds, told apart by Format. A decision record
// (RecordFormat) is the full account of one decision: Decide at any phase, and
// the package-level denial of Expose. An exposure record (ExposureFormat) is
// the compact account of what Expose decided for every declared capability; its
// per-capability content is Exposure, and the members that describe a single
// decision (CapabilityID, Checks, Risk, Requires, the field lists and the rules)
// are not part of it and are not encoded. Sinks that must tell them apart test
// IsExposure.
type Record struct {
	Format         string            `json:"format"`
	DecisionID     string            `json:"decision_id"`
	DecidedAt      time.Time         `json:"decided_at"`
	Phase          Phase             `json:"phase"`
	Outcome        Outcome           `json:"outcome"`
	Code           contracts.ErrCode `json:"code,omitempty"`
	Subject        Subject           `json:"subject"`
	CapabilityID   string            `json:"capability_id"`
	Package        PackageRef        `json:"package"`
	Policies       []PolicyRef       `json:"policies"`
	Checks         []Check           `json:"checks"`
	Risk           RiskRecord        `json:"risk"`
	Requires       Requirements      `json:"requires"`
	VisibleFields  []string          `json:"visible_fields"`
	WithheldFields []WithheldField   `json:"withheld_fields"`
	RulesApplied   []RuleApplied     `json:"rules_applied"`
	RulesDeferred  []string          `json:"rules_deferred"`

	// Exposure is the compact list of an exposure record; nil on a decision
	// record. It is encoded by MarshalJSON, never as a member of a decision
	// record.
	Exposure *ExposureBatch `json:"-"`
}

// CapabilityOutcome is what Expose decided for one capability: the entry of an
// exposure record. Code and Reason are set on a denial only: Code is the typed
// error code and Reason the fixed machine-readable reason of the check that
// denied it. Risk is the effective level (the highest level when the denial
// stopped before the level was known). It never holds a field value or text from
// a request.
type CapabilityOutcome struct {
	CapabilityID string              `json:"capability_id"`
	Outcome      Outcome             `json:"outcome"`
	Code         contracts.ErrCode   `json:"code,omitempty"`
	Reason       string              `json:"reason,omitempty"`
	Risk         contracts.RiskLevel `json:"risk"`
}

// ExposureBatch is the content of an exposure record. Considered is how many
// capabilities the Expose call evaluated in all; Outcomes lists those of this
// record, sorted by capability ID. When a catalogue does not fit one record the
// call writes Parts records, numbered from 1 in Part, whose Outcomes together
// list every capability once; a reader that finds fewer than Parts records for a
// call has found an incomplete audit trail, and the call exposed nothing to the
// model, whatever the parts that were written say: Expose shows capabilities
// only once every part is in the log.
type ExposureBatch struct {
	Considered int
	Part       int
	Parts      int
	Outcomes   []CapabilityOutcome
}

// IsExposure reports whether r is an exposure record rather than a decision
// record.
func (r Record) IsExposure() bool { return r.Format == ExposureFormat }

// exposureSubject is the subject of an exposure record: who asked, without the
// token's capability list, which an exposure record holds as its outcomes (a
// capability the token did not permit is a denial with the reason
// capability_not_in_token_scope).
type exposureSubject struct {
	ApplicationID string   `json:"application_id"`
	TenantID      string   `json:"tenant_id"`
	WorkspaceID   string   `json:"workspace_id"`
	PrincipalID   string   `json:"principal_id"`
	SessionID     string   `json:"session_id"`
	Roles         []string `json:"roles"`
}

// exposureWire is the encoding of an exposure record.
type exposureWire struct {
	Format     string              `json:"format"`
	DecisionID string              `json:"decision_id"`
	DecidedAt  time.Time           `json:"decided_at"`
	Phase      Phase               `json:"phase"`
	Outcome    Outcome             `json:"outcome"`
	Code       contracts.ErrCode   `json:"code,omitempty"`
	Subject    exposureSubject     `json:"subject"`
	Package    PackageRef          `json:"package"`
	Policies   []PolicyRef         `json:"policies"`
	Considered int                 `json:"considered"`
	Part       int                 `json:"part"`
	Parts      int                 `json:"parts"`
	Outcomes   []CapabilityOutcome `json:"outcomes"`
}

// MarshalJSON encodes a decision record exactly as the struct tags say and an
// exposure record in its compact form. A record that names the exposure format
// but holds no exposure does not encode: the audit append that carries it fails,
// which is the fail-closed outcome.
func (r Record) MarshalJSON() ([]byte, error) {
	if !r.IsExposure() {
		type plain Record // no methods: encodes by the tags
		return json.Marshal(plain(r))
	}
	if r.Exposure == nil {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "policy: an exposure record without outcomes")
	}
	w := exposureWire{
		Format: ExposureFormat, DecisionID: r.DecisionID, DecidedAt: r.DecidedAt, Phase: r.Phase,
		Outcome: r.Outcome, Code: r.Code,
		Subject: exposureSubject{
			ApplicationID: r.Subject.ApplicationID, TenantID: r.Subject.TenantID, WorkspaceID: r.Subject.WorkspaceID,
			PrincipalID: r.Subject.PrincipalID, SessionID: r.Subject.SessionID, Roles: r.Subject.Roles,
		},
		Package: r.Package, Policies: r.Policies,
		Considered: r.Exposure.Considered, Part: r.Exposure.Part, Parts: r.Exposure.Parts, Outcomes: r.Exposure.Outcomes,
	}
	if w.Subject.Roles == nil {
		w.Subject.Roles = []string{}
	}
	if w.Policies == nil {
		w.Policies = []PolicyRef{}
	}
	if w.Outcomes == nil {
		w.Outcomes = []CapabilityOutcome{}
	}
	return json.Marshal(w)
}

// UnmarshalJSON decodes either kind of record, so that a record read back from
// the audit log is the Record that was written. An exposure record decodes
// without the members it does not carry (the token's capability list, the checks
// and so on), which stay at their zero values.
func (r *Record) UnmarshalJSON(b []byte) error {
	var head struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	if head.Format != ExposureFormat {
		type plain Record
		var p plain
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		*r = Record(p)
		return nil
	}
	var w exposureWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*r = Record{
		Format: w.Format, DecisionID: w.DecisionID, DecidedAt: w.DecidedAt, Phase: w.Phase, Outcome: w.Outcome, Code: w.Code,
		Subject: Subject{
			ApplicationID: w.Subject.ApplicationID, TenantID: w.Subject.TenantID, WorkspaceID: w.Subject.WorkspaceID,
			PrincipalID: w.Subject.PrincipalID, SessionID: w.Subject.SessionID, Roles: w.Subject.Roles,
		},
		Package: w.Package, Policies: w.Policies,
		Exposure: &ExposureBatch{Considered: w.Considered, Part: w.Part, Parts: w.Parts, Outcomes: w.Outcomes},
	}
	return nil
}
