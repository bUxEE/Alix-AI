package main

// The types below are the Go form of schemas/bench/result.schema.json. The
// JSON tags are the only field names; every key is always written (no
// omitempty), and a value that is unknown or does not apply is a nil pointer,
// which marshals as null.

// FormatVersion is result.format_version.
const FormatVersion = 1

// Result is one benchmark run.
type Result struct {
	FormatVersion   int             `json:"format_version"`
	RunID           string          `json:"run_id"`
	Timestamp       string          `json:"timestamp"`
	DurationMS      float64         `json:"duration_ms"`
	Git             GitInfo         `json:"git"`
	Host            HostInfo        `json:"host"`
	Profile         string          `json:"profile"`
	Emulated        bool            `json:"emulated"`
	Synthetic       bool            `json:"synthetic"`
	ContainerLimits ContainerLimits `json:"container_limits"`
	Provider        ProviderInfo    `json:"provider"`
	Model           ModelInfo       `json:"model"`
	Backend         BackendInfo     `json:"backend"`
	Context         ContextInfo     `json:"context"`
	Corpus          CorpusInfo      `json:"corpus"`
	Config          ConfigInfo      `json:"config"`
	Metrics         MetricSet       `json:"metrics"`
	ByCategory      ByCategory      `json:"by_category"`
	ByLanguage      ByLanguage      `json:"by_language"`
	Latency         LatencyInfo     `json:"latency"`
	Memory          MemoryInfo      `json:"memory"`
	Errors          ErrorsInfo      `json:"errors"`
	Cases           []CaseOutcome   `json:"cases"`
}

// GitInfo describes the source tree.
type GitInfo struct {
	SHA   *string `json:"sha"`
	Dirty *bool   `json:"dirty"`
}

// HostInfo describes the machine that ran the tool.
type HostInfo struct {
	CPUModel     *string `json:"cpu_model"`
	LogicalCores int     `json:"logical_cores"`
	RAMMB        *int64  `json:"ram_mb"`
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
}

// ContainerLimits are the caps a profile stands for.
type ContainerLimits struct {
	MemMB  int     `json:"mem_mb"`
	CPUs   float64 `json:"cpus"`
	Source string  `json:"source"`
}

// ProviderInfo names the provider that answered.
type ProviderInfo struct {
	Name string `json:"name"`
}

// ModelInfo is the model and artefact that was measured.
type ModelInfo struct {
	RegistryID         string  `json:"registry_id"`
	InRegistry         bool    `json:"in_registry"`
	Name               *string `json:"name"`
	Status             *string `json:"status"`
	RuntimeModelName   *string `json:"runtime_model_name"`
	ArtifactSHA256     *string `json:"artifact_sha256"`
	ArtifactHashSource string  `json:"artifact_hash_source"`
	ArtifactSizeBytes  *int64  `json:"artifact_size_bytes"`
	Quantization       *string `json:"quantization"`
}

// Artifact hash sources.
const (
	HashComputed = "computed"
	HashRegistry = "registry"
	HashNone     = "none"
)

// BackendInfo is the runtime and inference backend.
type BackendInfo struct {
	Runtime        *string `json:"runtime"`
	Name           *string `json:"name"`
	RuntimeVersion *string `json:"runtime_version"`
}

// ContextInfo is the context size facts.
type ContextInfo struct {
	ConfiguredToks   *int `json:"configured_toks"`
	NativeMaxToks    *int `json:"native_max_toks"`
	PromptBudgetToks int  `json:"prompt_budget_toks"`
}

// CorpusInfo identifies the corpus.
type CorpusInfo struct {
	ID         string `json:"id"`
	SHA256     string `json:"sha256"`
	CasesTotal int    `json:"cases_total"`
	CasesRun   int    `json:"cases_run"`
}

// ConfigInfo is how the run was configured.
type ConfigInfo struct {
	Role             string     `json:"role"`
	Constraint       string     `json:"constraint"`
	Stream           bool       `json:"stream"`
	Temperature      float64    `json:"temperature"`
	MaxToks          int        `json:"max_toks"`
	TimeoutMS        int64      `json:"timeout_ms"`
	WarmupRequests   int        `json:"warmup_requests"`
	Limit            *int       `json:"limit"`
	Prompt           PromptInfo `json:"prompt"`
	PercentileMethod string     `json:"percentile_method"`
}

// PromptInfo identifies the prompt template.
type PromptInfo struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// Ratio is passed out of total; Rate is nil when Total is 0.
type Ratio struct {
	Passed int      `json:"passed"`
	Total  int      `json:"total"`
	Rate   *float64 `json:"rate"`
}

// newRatio builds a Ratio with its rate rounded to six decimals.
func newRatio(passed, total int) Ratio {
	r := Ratio{Passed: passed, Total: total}
	if total > 0 {
		r.Rate = round6(float64(passed) / float64(total))
	}
	return r
}

// MetricSet is the metrics over some set of cases (see the schema for each
// metric's definition and denominator).
type MetricSet struct {
	Cases               int   `json:"cases"`
	SchemaValid         Ratio `json:"schema_valid"`
	CapabilityRouting   Ratio `json:"capability_routing"`
	Intent              Ratio `json:"intent"`
	FieldExactMatch     Ratio `json:"field_exact_match"`
	FieldValueAccuracy  Ratio `json:"field_value_accuracy"`
	CatalogueConformity Ratio `json:"catalogue_conformance"`
	RefusalCorrectness  Ratio `json:"refusal_correctness"`
	InjectionResistance Ratio `json:"injection_resistance"`
	Pass                Ratio `json:"pass"`
}

// ByCategory is the metrics per category.
type ByCategory struct {
	Routing    MetricSet `json:"routing"`
	Extraction MetricSet `json:"extraction"`
	Refusal    MetricSet `json:"refusal"`
	Injection  MetricSet `json:"injection"`
}

// ByLanguage is the metrics per language.
type ByLanguage struct {
	EN MetricSet `json:"en"`
	IT MetricSet `json:"it"`
}

// LatencyInfo is the latency and throughput block.
type LatencyInfo struct {
	TotalMS              Stats    `json:"total_ms"`
	TTFTMS               Stats    `json:"ttft_ms"`
	PromptToksEst        Stats    `json:"prompt_toks_est"`
	ToksIn               int      `json:"toks_in"`
	ToksOut              int      `json:"toks_out"`
	ToksPerSecond        *float64 `json:"toks_per_second"`
	DecodeToksPerSecond  *float64 `json:"decode_toks_per_second"`
	WarmupFirstRequestMS *float64 `json:"warmup_first_request_ms"`
}

// Memory sources.
const (
	MemSourceDocker = "docker-stats"
	MemSourceNone   = "none"
)

// MemoryInfo is the memory sampling block.
type MemoryInfo struct {
	Sampled         bool     `json:"sampled"`
	Source          string   `json:"source"`
	Container       *string  `json:"container"`
	IntervalMS      *int64   `json:"interval_ms"`
	Samples         int      `json:"samples"`
	SampleErrors    int      `json:"sample_errors"`
	BaselineMB      *float64 `json:"baseline_mb"`
	PeakRSSMB       *float64 `json:"peak_rss_mb"`
	SteadyRSSMB     *float64 `json:"steady_rss_mb"`
	LimitMBObserved *float64 `json:"limit_mb_observed"`
}

// ErrorsInfo counts the cases that ended in an error.
type ErrorsInfo struct {
	Count  int            `json:"count"`
	ByCode map[string]int `json:"by_code"`
}

// Case statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// CaseOutcome is the raw outcome of one case: what the runtime did and every
// flag the scorer derived from it. The aggregate metrics are a pure function
// of a slice of these (Aggregate), which is how `bench check` verifies them.
type CaseOutcome struct {
	ID                   string   `json:"id"`
	Language             string   `json:"language"`
	Category             string   `json:"category"`
	Status               string   `json:"status"`
	ErrorCode            *string  `json:"error_code"`
	LatencyMS            *float64 `json:"latency_ms"`
	TTFTMS               *float64 `json:"ttft_ms"`
	ToksIn               *int     `json:"toks_in"`
	ToksOut              *int     `json:"toks_out"`
	ToksEstimated        bool     `json:"toks_estimated"`
	FinishReason         *string  `json:"finish_reason"`
	PromptToksEst        int      `json:"prompt_toks_est"`
	Output               *string  `json:"output"`
	OutputTruncated      bool     `json:"output_truncated"`
	ProposalCapabilityID *string  `json:"proposal_capability_id"`
	ProposalIntent       *string  `json:"proposal_intent"`
	SchemaValid          bool     `json:"schema_valid"`
	SchemaError          *string  `json:"schema_error"`
	IntentOK             bool     `json:"intent_ok"`
	CapabilityOK         *bool    `json:"capability_ok"`
	FieldsExact          *bool    `json:"fields_exact"`
	FieldHits            int      `json:"field_hits"`
	FieldTotal           int      `json:"field_total"`
	CatalogueOK          bool     `json:"catalogue_ok"`
	RefusalOK            *bool    `json:"refusal_ok"`
	Resisted             *bool    `json:"resisted"`
	AttackSucceeded      *bool    `json:"attack_succeeded"`
	Pass                 bool     `json:"pass"`
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }
