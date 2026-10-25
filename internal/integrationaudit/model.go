package integrationaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Formats of the documents this package defines. A change to what is signed is
// a new version, never an edit.
const (
	// ReportFormat is the format member of a Report.
	ReportFormat = "alixai.integration-audit-report/v1"
	// AttestationFormat is the format member of the signed attestation. The
	// platform key that signs it also signs audit-log checkpoints and release
	// manifests, so the signed payload carries its own format: a signature made
	// over a document of another kind never verifies as an attestation.
	AttestationFormat = "alixai.integration-audit-attestation/v1"
)

// Status is the result of a check, a section or a target.
//
// "unverified" is not "pass": it says the audit could not look. Only a status
// of pass says something was looked at and was right.
type Status string

// The four statuses.
const (
	StatusPass       Status = "pass"
	StatusFail       Status = "fail"
	StatusUnverified Status = "unverified"
	// StatusNotRun means no check was registered for a section or target, so
	// nothing at all is known about it.
	StatusNotRun Status = "not_run"
)

// worse returns the more severe of two statuses on the scale
// fail > unverified > pass > not_run.
func worse(a, b Status) Status {
	if rankStatus(b) > rankStatus(a) {
		return b
	}
	return a
}

func rankStatus(s Status) int {
	switch s {
	case StatusFail:
		return 3
	case StatusUnverified:
		return 2
	case StatusPass:
		return 1
	}
	return 0
}

// Target is one of the three audit targets of master plan 4.5.
type Target string

// The three targets.
const (
	// TargetPackage: signed manifest, schemas, documentation, contracts,
	// adapter configuration, policies and source declarations.
	TargetPackage Target = "package"
	// TargetRuntime: the reachable service, its versions, modules,
	// dependencies, health, scopes, credential references, limits, storage and
	// network permissions, as the application exposes them for inspection.
	TargetRuntime Target = "runtime"
	// TargetApplication: deterministic reads and results, validation and
	// prepare/commit, authorisation, RAG source coverage, documentation
	// completeness and the complete approved control surface.
	TargetApplication Target = "application"
)

// Targets lists the three targets in report order.
func Targets() []Target { return []Target{TargetPackage, TargetRuntime, TargetApplication} }

// Valid reports whether t is one of the three targets.
func (t Target) Valid() bool {
	return t == TargetPackage || t == TargetRuntime || t == TargetApplication
}

// Stage is a step of the activation workflow of master plan 9.4. Checks run in
// stage order; the signed report and the activation decision follow the last.
type Stage int

// The stages of master plan 9.4, in order.
const (
	// StageSignature: package signature and manifest validation.
	StageSignature Stage = iota + 1
	// StageDocumentation: documentation and conversation-contract coverage.
	StageDocumentation
	// StageInventory: capability, schema, MCP and API inventory.
	StageInventory
	// StageResults: deterministic-result and action-receipt probes.
	StageResults
	// StageScope: tenant, workspace and scope isolation probes.
	StageScope
	// StageRAG: authorised RAG-source and citation probes.
	StageRAG
	// StageSafety: failure, replay and stale-state probes.
	StageSafety
	// StageBenchmark: resource and latency benchmark on the selected profile.
	StageBenchmark
)

// Valid reports whether s is one of the eight stages.
func (s Stage) Valid() bool { return s >= StageSignature && s <= StageBenchmark }

// String names the stage.
func (s Stage) String() string {
	switch s {
	case StageSignature:
		return "signature"
	case StageDocumentation:
		return "documentation"
	case StageInventory:
		return "inventory"
	case StageResults:
		return "results"
	case StageScope:
		return "scope"
	case StageRAG:
		return "rag"
	case StageSafety:
		return "safety"
	case StageBenchmark:
		return "benchmark"
	}
	return "unknown"
}

// Section numbers the ten required checks of master plan 4.5. A check that is
// not one of them (a supplementary check) has section 0.
const (
	SectionAuthenticity   = 1
	SectionDeterministic  = 2
	SectionRAGInventory   = 3
	SectionDocumentation  = 4
	SectionContracts      = 5
	SectionScope          = 6
	SectionControlSurface = 7
	SectionSafeExecution  = 8
	SectionDegradation    = 9
	SectionRuntimeSmoke   = 10
	sectionCount          = 10
	sectionSupplementary  = 0
)

var sectionTitles = [sectionCount + 1]string{
	"", // supplementary
	"Package authenticity",
	"Deterministic results",
	"RAG source inventory",
	"Documentation completeness",
	"Conversation contracts",
	"Exact scope enforcement",
	"Complete control surface",
	"Safe execution",
	"Failure and degradation",
	"Runtime smoke test",
}

// SectionTitle returns the title master plan 4.5 gives check n, or "" for
// anything else.
func SectionTitle(n int) string {
	if n < 1 || n > sectionCount {
		return ""
	}
	return sectionTitles[n]
}

// Subject identifies exactly what was audited: the application and package
// versions and hashes. It holds nothing that changes when the package is
// activated, so a re-audit of the same bytes gives the same subject.
type Subject struct {
	ApplicationID      string `json:"application_id"`
	ApplicationVersion string `json:"application_version"`
	PackageVersion     string `json:"package_version"`
	// ManifestHash is the content address of the package: the SHA-256 of the
	// canonical signed integrity manifest.
	ManifestHash string `json:"manifest_hash"`
	// CoreVersion is the AlixAI core version the package was checked against.
	CoreVersion    string `json:"core_version"`
	SignerKeyID    string `json:"signer_key_id"`
	SecurityDomain string `json:"security_domain,omitempty"`
	Files          int    `json:"files"`
	Bytes          int64  `json:"bytes"`
}

// Profile is the model and runtime profile the audit ran on (master plan 4.5:
// "model/runtime profile used"). The audit asks no model anything; the profile
// is what the runtime probes of M1-18 and the resource benchmark run against.
type Profile struct {
	// Name is the configured profile, or "" when none was given.
	Name string `json:"name"`
	// Minimum is the manifest's model_requirements.minimum_profile.
	Minimum string `json:"minimum"`
}

// TargetResult is the status of one audit target.
type TargetResult struct {
	Target Target `json:"target"`
	Status Status `json:"status"`
	// Checks are the IDs of the checks that audited the target, sorted.
	Checks []string `json:"checks"`
	// Unverified lists what the target's checks could not look at.
	Unverified []string `json:"unverified"`
}

// SectionResult is the status of one of the ten checks of master plan 4.5.
type SectionResult struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	// Checks are the IDs of the checks that implement the section, sorted.
	Checks []string `json:"checks"`
}

// CheckRecord says that a check ran and how it came out.
type CheckRecord struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Section  int    `json:"section"`
	Stage    string `json:"stage"`
	Target   Target `json:"target"`
	Status   Status `json:"status"`
	Critical int    `json:"critical"`
	Warnings int    `json:"warnings"`
	Info     int    `json:"info"`
	// Unverified counts the items the check could not inspect.
	Unverified int `json:"unverified"`
}

// DeclaredOperation is one adapter call the package maps a capability
// operation to.
type DeclaredOperation struct {
	Capability string `json:"capability"`
	Operation  string `json:"operation"`
	Endpoint   string `json:"endpoint"`
	// Call is the MCP tool, OpenAPI operationId or native method.
	Call string `json:"call"`
}

// DiscoveredCapability is what an adapter's Describe reports for one
// capability: the operations it can perform.
type DiscoveredCapability struct {
	CapabilityID string   `json:"capability_id"`
	Operations   []string `json:"operations"`
}

// EndpointRecord is one endpoint the manifest declares.
type EndpointRecord struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
}

// Surface is the control surface: what the package declares and what the
// adapter discovered (master plan 4.5: "adapters, MCP servers and API
// operations discovered").
type Surface struct {
	// Inspected is true when an adapter described itself. False means the
	// discovered side is empty because nothing was inspected, not because the
	// application exposes nothing.
	Inspected          bool                   `json:"inspected"`
	Transport          string                 `json:"transport"`
	ContractVersions   []int                  `json:"contract_versions"`
	ApplicationVersion string                 `json:"application_version"`
	Endpoints          []EndpointRecord       `json:"endpoints"`
	Declared           []DeclaredOperation    `json:"declared"`
	Discovered         []DiscoveredCapability `json:"discovered"`
	// Unmapped are host operations no capability maps.
	Unmapped []string `json:"unmapped"`
}

// CapabilityStatus is the audit's verdict on one capability.
type CapabilityStatus string

// The verdicts of master plan 4.5 ("capabilities passed, failed, skipped or
// quarantined") plus unverified.
const (
	// CapabilityPassed: every check that examined the capability passed.
	CapabilityPassed CapabilityStatus = "passed"
	// CapabilityFailed: a critical finding concerns the capability.
	CapabilityFailed CapabilityStatus = "failed"
	// CapabilityQuarantined: a check withheld the capability from service
	// without failing the whole application.
	CapabilityQuarantined CapabilityStatus = "quarantined"
	// CapabilityUnverified: a check could not inspect the capability.
	CapabilityUnverified CapabilityStatus = "unverified"
	// CapabilitySkipped: no check examined the capability.
	CapabilitySkipped CapabilityStatus = "skipped"
)

// Valid reports whether s is one of the five verdicts.
func (s CapabilityStatus) Valid() bool {
	switch s {
	case CapabilityPassed, CapabilityFailed, CapabilityQuarantined, CapabilityUnverified, CapabilitySkipped:
		return true
	}
	return false
}

// rank orders verdicts, worst first: failed, quarantined, unverified, passed,
// skipped. A skipped verdict never makes a capability look worse; it only says
// nothing examined it.
func (s CapabilityStatus) rank() int {
	switch s {
	case CapabilityFailed:
		return 4
	case CapabilityQuarantined:
		return 3
	case CapabilityUnverified:
		return 2
	case CapabilityPassed:
		return 1
	}
	return 0
}

// CapabilityResult is the audit's verdict on one declared capability.
type CapabilityResult struct {
	ID         string           `json:"id"`
	Audience   string           `json:"audience"`
	RiskLevel  int              `json:"risk_level"`
	Operations []string         `json:"operations"`
	Status     CapabilityStatus `json:"status"`
	// Findings are the keys of the findings that concern the capability.
	Findings []string `json:"findings"`
	// Notes are the reasons explicit verdicts were given (quarantined,
	// unverified, skipped), sorted.
	Notes []string `json:"notes"`
}

// SourceResult is the audit's record of one declared RAG source: its owner, its
// exact scope and which of the fields master plan 4.5 check 3 requires are
// declared.
type SourceResult struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Owner string `json:"owner"`
	// Scope is the declared scope, for example
	// "application=demo tenant=request workspace=request".
	Scope string `json:"scope"`
	// Declared and Missing partition the fields of check 3.
	Declared []string `json:"declared"`
	Missing  []string `json:"missing"`
	// Verified says how far the source was verified: "declaration" (the
	// declaration is complete) or "live" (a probe reached it, M2-12).
	Verified string `json:"verified"`
}

// CoverageItem counts how much of something the audit covered.
type CoverageItem struct {
	// Name is a stable identifier such as "documentation_facets".
	Name    string `json:"name"`
	Covered int    `json:"covered"`
	Total   int    `json:"total"`
}

// ProbeKind names a kind of probe result. M1-18 and M2-12 use these.
type ProbeKind string

// Probe kinds the framework knows by name; a check may add its own.
const (
	// ProbeNegativeScope: a cross-scope request that must be refused.
	ProbeNegativeScope ProbeKind = "negative_scope"
	// ProbePositiveScope: an in-scope request that must succeed.
	ProbePositiveScope ProbeKind = "positive_scope"
	// ProbeSafeExecution: a prepare, confirm or commit safety probe.
	ProbeSafeExecution ProbeKind = "safe_execution"
	// ProbeDeterministicResult: a typed-result shape probe.
	ProbeDeterministicResult ProbeKind = "deterministic_result"
	// ProbeSmoke: a step of the runtime smoke test.
	ProbeSmoke ProbeKind = "smoke"
)

// ProbeResult is the outcome of one probe.
type ProbeResult struct {
	Kind       ProbeKind `json:"kind"`
	Capability string    `json:"capability"`
	// Subject says what was probed, for example a scope pair.
	Subject  string `json:"subject"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	Passed   bool   `json:"passed"`
}

// Measurement is a resource or latency measurement in integer units (the signed
// document admits no floating point). A probe that measures wall-clock time
// should record a coarse bucket: two audits of the same inputs are
// byte-identical only as far as their measurements are.
type Measurement struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Value int64  `json:"value"`
}

// Unverified is something the audit could not inspect. It is never a pass.
type Unverified struct {
	// Subject says what could not be inspected, for example "aspect:storage".
	Subject string `json:"subject"`
	// Capability is set when the item concerns one capability.
	Capability string `json:"capability,omitempty"`
	// Section is the master plan 4.5 check number, or 0.
	Section int `json:"section"`
	// Target is the audit target the item belongs to.
	Target Target `json:"target"`
	Reason string `json:"reason"`
}

// Permissions is the audit's reading of the permission model: what is missing,
// excessive or contradictory (master plan 4.5, "Audit result").
type Permissions struct {
	Missing       []string `json:"missing"`
	Excessive     []string `json:"excessive"`
	Contradictory []string `json:"contradictory"`
}

// ExternalSource is one RAG source whose egress mode is not local_only.
type ExternalSource struct {
	ID             string `json:"id"`
	Egress         string `json:"egress"`
	Classification string `json:"classification"`
}

// ExternalField is one capability field classified above internal.
type ExternalField struct {
	Capability     string `json:"capability"`
	Field          string `json:"field"`
	Classification string `json:"classification"`
}

// External lists what could leave the deployment if external provider mode
// were enabled (additive plan: the package can only restrict). It is
// informational: the audit never enables or disables the mode.
type External struct {
	// DeclaredByPackage is true when the package declares any egress mode,
	// classification or external block. A package that declares none is read
	// with the safe defaults: every source local_only, every field internal.
	DeclaredByPackage bool `json:"declared_by_package"`
	// ForbidExternal is the package's external.forbid.
	ForbidExternal bool             `json:"forbid_external"`
	Sources        []ExternalSource `json:"sources"`
	Fields         []ExternalField  `json:"fields"`
}

// Summary counts the findings and says what the report permits.
type Summary struct {
	Critical   int `json:"critical"`
	Warnings   int `json:"warnings"`
	Info       int `json:"info"`
	Unverified int `json:"unverified"`
	// Outcome is "blocked" (a critical finding: the package stays in
	// quarantine), "needs_acceptance" (no critical finding, but warnings an
	// authorised operator must accept) or "clear".
	Outcome Disposition `json:"outcome"`
}

// Disposition is what a report permits.
type Disposition string

// The three dispositions.
const (
	DispositionBlocked         Disposition = "blocked"
	DispositionNeedsAcceptance Disposition = "needs_acceptance"
	DispositionClear           Disposition = "clear"
)

// Valid reports whether o is one of the three outcomes.
func (o Disposition) Valid() bool {
	return o == DispositionBlocked || o == DispositionNeedsAcceptance || o == DispositionClear
}

// KeyedFinding is a finding with the stable key an operator uses to accept it.
type KeyedFinding struct {
	// Key identifies the finding: a digest of its check, capability, path,
	// severity and message. It is bound to the package by the acceptance
	// record, so accepting a finding of one package accepts nothing in another.
	Key string `json:"key"`
	contracts.Finding
}

// Remediation is one exact item the operator must act on.
type Remediation struct {
	Key         string             `json:"key"`
	CheckID     string             `json:"check_id"`
	Severity    contracts.Severity `json:"severity"`
	Capability  string             `json:"capability,omitempty"`
	Remediation string             `json:"remediation"`
}

// TriggerKind says which part of the integration a re-audit trigger watches.
type TriggerKind string

// The kinds of change that call for a new audit (master plan 4.5, first
// paragraph). Enforcement arrives in M5-10; this task records the hashes.
const (
	TriggerPackage            TriggerKind = "package"
	TriggerDocumentation      TriggerKind = "documentation"
	TriggerContract           TriggerKind = "conversation_contract"
	TriggerCapability         TriggerKind = "capability"
	TriggerSource             TriggerKind = "source_definition"
	TriggerPermissionModel    TriggerKind = "permission_model"
	TriggerAdapterSurface     TriggerKind = "adapter_surface"
	TriggerApplicationVersion TriggerKind = "application_version"
	TriggerCoreVersion        TriggerKind = "core_version"
)

// Trigger is one watched value: when its hash differs from the one recorded,
// the integration must be audited again.
type Trigger struct {
	Kind    TriggerKind `json:"kind"`
	Subject string      `json:"subject"`
	Hash    string      `json:"hash"`
}

// Extension is data a check adds to the report under its own ID. It is part of
// the signed document, so it is limited to what the canonical form admits:
// strings, integers, booleans, null, arrays and objects, no floats.
type Extension struct {
	Check string         `json:"check"`
	Data  map[string]any `json:"data"`
}

// Report is the audit result of master plan 4.5. It is deterministic: it holds
// no clock reading, no random identifier and no mutable state of the package
// (not its status, not the report that activated it), every list is sorted and
// every number is an integer, so two audits of the same inputs produce
// byte-identical reports. When it was made, and by which key, is the business
// of the Attestation that signs it.
type Report struct {
	Format          string             `json:"format"`
	Subject         Subject            `json:"subject"`
	Profile         Profile            `json:"profile"`
	Targets         []TargetResult     `json:"targets"`
	Sections        []SectionResult    `json:"sections"`
	Checks          []CheckRecord      `json:"checks"`
	Surface         Surface            `json:"surface"`
	Capabilities    []CapabilityResult `json:"capabilities"`
	RAGSources      []SourceResult     `json:"rag_sources"`
	Coverage        []CoverageItem     `json:"coverage"`
	Probes          []ProbeResult      `json:"probes"`
	Permissions     Permissions        `json:"permissions"`
	External        External           `json:"external"`
	Measurements    []Measurement      `json:"measurements"`
	Findings        []KeyedFinding     `json:"findings"`
	Unverified      []Unverified       `json:"unverified"`
	Summary         Summary            `json:"summary"`
	Remediation     []Remediation      `json:"remediation"`
	ReauditTriggers []Trigger          `json:"reaudit_triggers"`
	Extensions      []Extension        `json:"extensions"`
}

// FindingKey returns the key of a finding: the first 16 hex characters of the
// SHA-256 of its severity, check, capability, path and message. The
// remediation text is not part of the key: it is fixed wording per check.
func FindingKey(f contracts.Finding) string {
	h := sha256.New()
	for _, s := range []string{string(f.Severity), f.CheckID, f.Capability, f.Path, f.Message} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Digest returns the lower-case hex SHA-256 of the canonical JSON of v.
func Digest(v any) (string, error) {
	canon, err := canonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// jsonDigest hashes the plain JSON encoding of v. encoding/json is
// deterministic for the typed values it is used on (struct fields in order, map
// keys sorted), which is all a re-audit trigger needs: the same typed value
// hashes the same, and a changed one does not.
func jsonDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("unencodable")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
