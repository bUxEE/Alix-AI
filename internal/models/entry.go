// Package models loads, validates and verifies the model registry: one YAML
// file per model in models/registry/, described by
// schemas/models/registry-entry.schema.json (master plan 8.3, 16.3).
//
// The registry is the only authority on which model artefacts AlixAI may run.
// Loading is strict and fails closed: an unknown field, a missing licence, a
// certified entry without a pinned hash or evidence, two defaults for one
// (role, profile), or a broken fallback chain rejects the whole registry
// rather than loading the part that happens to be valid.
//
// Verify checks an installed artefact's content hash against its entry before
// the inference path is allowed to use it (SEC-063).
package models

import (
	"fmt"
)

// Status is an entry's place in the qualification lifecycle:
// candidate -> qualified-emulated -> certified, with rejected and retired as
// the two exits.
type Status string

const (
	// StatusCandidate: named by the master plan, not yet measured. Its
	// artefact hash may still be unpinned.
	StatusCandidate Status = "candidate"
	// StatusQualifiedEmulated: passed the qualification suite in a
	// resource-limited container. Never a certification.
	StatusQualifiedEmulated Status = "qualified-emulated"
	// StatusCertified: qualified on real hardware with a pinned hash,
	// a licence and evidence.
	StatusCertified Status = "certified"
	// StatusRejected: evaluated and not acceptable.
	StatusRejected Status = "rejected"
	// StatusRetired: a certified entry that is no longer supported.
	StatusRetired Status = "retired"
)

// AllStatuses lists every defined status in lifecycle order.
func AllStatuses() []Status {
	return []Status{StatusCandidate, StatusQualifiedEmulated, StatusCertified, StatusRejected, StatusRetired}
}

// Valid reports whether s is a defined status.
func (s Status) Valid() bool {
	for _, v := range AllStatuses() {
		if s == v {
			return true
		}
	}
	return false
}

// transitions is the lifecycle graph. There is no way to skip qualification:
// candidate cannot become certified directly.
var transitions = map[Status][]Status{
	StatusCandidate:         {StatusQualifiedEmulated, StatusRejected},
	StatusQualifiedEmulated: {StatusCertified, StatusRejected},
	// A certified entry can be demoted when a re-check fails, or retired.
	StatusCertified: {StatusQualifiedEmulated, StatusRejected, StatusRetired},
	// A rejected model can be evaluated again (new revision, new quantisation).
	StatusRejected: {StatusCandidate},
	StatusRetired:  {},
}

// CanTransition reports whether an entry may move from s to next. An
// undefined status on either side is never allowed. The registry files carry
// only the current status, so this is for the tools that edit them (the
// benchmark spike, the release pipeline), not for the loader.
func (s Status) CanTransition(next Status) bool {
	for _, to := range transitions[s] {
		if to == next {
			return true
		}
	}
	return false
}

// Role is what a model is used for (master plan 8.2).
type Role string

const (
	RoleTypedRouting             Role = "typed-routing"
	RoleConversationalController Role = "conversational-controller"
	RoleEmbedding                Role = "embedding"
	RoleReranker                 Role = "reranker"
	RoleSTT                      Role = "stt"
	RoleTTS                      Role = "tts"
	// RoleDecisionScorer scores a bounded set of authorised candidates for the
	// optional decision layer (master plan 8.5). Advisory only.
	RoleDecisionScorer Role = "decision-scorer"
	// RoleDecisionEncoder is a frozen encoder a bi-encoder scorer depends on.
	// It is a dependency, never selected and never a default.
	RoleDecisionEncoder Role = "decision-encoder"
)

// Profile is a hardware profile (master plan section 7).
type Profile string

const (
	ProfileLite           Profile = "lite"
	ProfileBalanced       Profile = "balanced"
	ProfileExperimental4G Profile = "experimental-4g"
	// ProfileHigh is 32 GB / 8 CPU, the profile the optional CLM scorer is
	// measured on (tools/decbench).
	ProfileHigh Profile = "high"
)

// Entry is one registry file. Its JSON form is
// schemas/models/registry-entry.schema.json; the YAML file is converted to
// that JSON form before it is validated and decoded, so these tags are the
// only field names. No field has omitempty: the schema requires every key and
// an unknown value is an explicit null.
type Entry struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	Status       Status     `json:"status"`
	Roles        []Role     `json:"roles"`
	Capabilities []string   `json:"capabilities"`
	Upstream     Upstream   `json:"upstream"`
	Artifact     Artifact   `json:"artifact"`
	Licence      Licence    `json:"licence"`
	Quantization Quant      `json:"quantization"`
	Backend      Backend    `json:"backend"`
	Hardware     Hardware   `json:"hardware"`
	Memory       Memory     `json:"memory"`
	Context      Context    `json:"context"`
	LoadTime     LoadTime   `json:"load_time"`
	Performance  Perf       `json:"performance"`
	Languages    Languages  `json:"languages"`
	Scores       Scores     `json:"scores"`
	Measurement  Measure    `json:"measurement"`
	Fallback     Fallback   `json:"fallback"`
	Review       Review     `json:"last_efficiency_review"`
	DefaultFor   []Default  `json:"default_for"`
	Embedding    *Embedding `json:"embedding,omitempty"`
	// Dependencies and Scorer are revision 2 (D-11): optional for every role
	// except that a decision-scorer carries Scorer.
	Dependencies []Dependency `json:"dependencies,omitempty"`
	Scorer       *Scorer      `json:"scorer,omitempty"`
	Notes        []string     `json:"notes"`
}

// Upstream is the model as its authors published it.
type Upstream struct {
	RepoURL  string  `json:"repo_url"`
	Revision *string `json:"revision"`
}

// Artifact is the exact file the runtime loads.
type Artifact struct {
	Format      string  `json:"format"`
	RepoURL     *string `json:"repo_url"`
	Filename    *string `json:"filename"`
	Revision    *string `json:"revision"`
	SizeBytes   *int64  `json:"size_bytes"`
	SHA256      *string `json:"sha256"`
	QuantizedBy string  `json:"quantized_by"`
	FirstParty  bool    `json:"first_party"`
	CheckedOn   string  `json:"checked_on"`
}

// Licence is SEC-091's record: SPDX id, URL and redistribution notes.
type Licence struct {
	SPDX                string `json:"spdx"`
	URL                 string `json:"url"`
	RedistributionNotes string `json:"redistribution_notes"`
}

// Quant is the quantisation scheme.
type Quant struct {
	Scheme      string `json:"scheme"`
	NominalBits int    `json:"nominal_bits"`
}

// Backend is the runtime that loads the artefact.
type Backend struct {
	Runtime string `json:"runtime"`
	Name    string `json:"name"`
	// AllowedRuntimeVersions are the runtime versions this artefact was
	// qualified with. Empty denies every version (fail closed).
	AllowedRuntimeVersions []string `json:"allowed_runtime_versions"`
}

// HardwareSpec is one host description. Nil means not yet measured.
type HardwareSpec struct {
	CPUCores    *int    `json:"cpu_cores"`
	RAMMB       *int    `json:"ram_mb"`
	Accelerator *string `json:"accelerator"`
}

// Hardware is the minimum and recommended host.
type Hardware struct {
	Minimum     HardwareSpec `json:"minimum"`
	Recommended HardwareSpec `json:"recommended"`
}

// Memory is resident set size in MiB.
type Memory struct {
	PeakRSSMB   *float64 `json:"peak_rss_mb"`
	SteadyRSSMB *float64 `json:"steady_rss_mb"`
}

// Context is the context window facts.
type Context struct {
	NativeMaxTokens *int     `json:"native_max_tokens"`
	BudgetTokens    *int     `json:"budget_tokens"`
	KVCacheMB       *float64 `json:"kv_cache_mb"`
}

// LoadTime is the model load time in milliseconds.
type LoadTime struct {
	ColdMS *float64 `json:"cold_ms"`
	WarmMS *float64 `json:"warm_ms"`
}

// Perf is latency and throughput.
type Perf struct {
	LatencyMSP50    *float64 `json:"latency_ms_p50"`
	LatencyMSP95    *float64 `json:"latency_ms_p95"`
	TTFTMSP50       *float64 `json:"ttft_ms_p50"`
	TokensPerSecond *float64 `json:"tokens_per_second"`
}

// Languages is what the upstream card declares and what AlixAI qualified.
type Languages struct {
	Declared     []string `json:"declared"`
	ClaimedCount *int     `json:"claimed_count"`
	Qualified    []string `json:"qualified"`
}

// Scores are qualification scores in [0, 1]; nil until measured.
type Scores struct {
	Task   *float64 `json:"task"`
	Tool   *float64 `json:"tool"`
	RAG    *float64 `json:"rag"`
	Safety *float64 `json:"safety"`
}

// Measure is the provenance of the measured fields.
type Measure struct {
	Profile     *Profile `json:"profile"`
	Emulated    *bool    `json:"emulated"`
	ResultsPath *string  `json:"results_path"`
	MeasuredOn  *string  `json:"measured_on"`
}

// Fallback is the fallback and degraded-mode behaviour.
type Fallback struct {
	ModelID      *string `json:"model_id"`
	DegradedMode string  `json:"degraded_mode"`
	Notes        *string `json:"notes"`
}

// Review is the last model-efficiency review (master plan 8.3).
type Review struct {
	Date         *string `json:"date"`
	EvidencePath *string `json:"evidence_path"`
}

// Default marks an entry as the default for one (role, profile).
type Default struct {
	Role    Role    `json:"role"`
	Profile Profile `json:"profile"`
}

// Embedding holds the facts an index layout depends on.
type Embedding struct {
	Dimensions     *int `json:"dimensions"`
	MaxInputTokens *int `json:"max_input_tokens"`
}

// Dependency names another registry entry this one cannot run without.
type Dependency struct {
	ID       string `json:"id"`
	Relation string `json:"relation"`
}

// DependencyFrozenEncoder is the only relation: the dependency is the frozen
// encoder whose output this model's heads read.
const DependencyFrozenEncoder = "frozen-encoder"

// Scorer architectures.
const (
	ScorerNonAutoregressiveEncoder = "non-autoregressive-encoder"
	ScorerBiEncoder                = "bi-encoder"
	ScorerAutoregressiveLetter     = "autoregressive-letter"
)

// Scorer holds the facts of a decision scorer. MaxOptions is nil when the
// source states no number (unverified).
type Scorer struct {
	Architecture string `json:"architecture"`
	MaxOptions   *int   `json:"max_options"`
}

// HasRole reports whether the entry lists role.
func (e *Entry) HasRole(role Role) bool {
	for _, r := range e.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// String is a short label for logs: "<id> (<status>)".
func (e *Entry) String() string { return fmt.Sprintf("%s (%s)", e.ID, e.Status) }
