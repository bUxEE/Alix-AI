package main

// The types below are the Go form of schemas/bench/decision-result.schema.json. The JSON
// tags are the only field names; every key is always written (no omitempty), and a value
// that is unknown or does not apply is a nil pointer, which marshals as null.
//
// One result file holds one run: the shared envelope (host, profile, corpus, scorer) and
// one entry of Configurations per configuration that ran, all on identical fixtures.

// FormatVersion is result.format_version.
const FormatVersion = 1

// Result is one decbench run.
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
	Scorer          ScorerInfo      `json:"scorer"`
	Controller      ControllerInfo  `json:"controller"`
	Corpus          CorpusInfo      `json:"corpus"`
	Config          RunConfig       `json:"config"`
	Live            LiveInfo        `json:"live"`
	Configurations  []Configuration `json:"configurations"`
}

// GitInfo describes the source tree.
type GitInfo struct {
	SHA   *string `json:"sha"`
	Dirty *bool   `json:"dirty"`
}

// HostInfo describes the machine that ran the tool.
type HostInfo struct {
	LogicalCores int    `json:"logical_cores"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
}

// ContainerLimits are the caps a profile stands for.
type ContainerLimits struct {
	MemMB  int     `json:"mem_mb"`
	CPUs   float64 `json:"cpus"`
	Source string  `json:"source"`
}

// ScorerInfo identifies the scorer behind configurations 2, 3 and 5.
type ScorerInfo struct {
	// Kind is fake, localai or clm.
	Kind           string  `json:"kind"`
	ProviderID     string  `json:"provider_id"`
	ModelID        string  `json:"model_id"`
	ModelRevision  string  `json:"model_revision"`
	ArtifactSHA256 *string `json:"artifact_sha256"`
	// ArtifactHashSource is computed (hashed from --artifact) or none.
	ArtifactHashSource string `json:"artifact_hash_source"`
}

// ControllerInfo says where the controller numbers come from. The controller is never a
// live model in this tool: it is scripted (synthetic stand-in) or its measured per-call
// latency is supplied from a live run (tools/bench), and the proposal size is always an
// estimate (bytes/3, see providers.EstimateTokens).
type ControllerInfo struct {
	// Source is scripted or supplied.
	Source string `json:"source"`
	// LatencyMS is the supplied per-call latency; nil when scripted.
	LatencyMS *float64 `json:"latency_ms"`
}

// CorpusInfo identifies the corpus.
type CorpusInfo struct {
	ID         string `json:"id"`
	SHA256     string `json:"sha256"`
	CasesTotal int    `json:"cases_total"`
}

// RunConfig is how the run was configured.
type RunConfig struct {
	MinConfidence    float64 `json:"min_confidence"`
	TimeoutMS        int64   `json:"timeout_ms"`
	CacheEntries     int     `json:"cache_entries"`
	PercentileMethod string  `json:"percentile_method"`
	CalibrationBins  int     `json:"calibration_bins"`
}

// LiveInfo holds the numbers a fake run cannot produce. Every field is nil unless the
// operator supplied a value measured elsewhere (a live host run); the tool never invents
// one. MemoryReserveMB is derived: the profile's memory cap minus peak RSS and resident
// model memory, and only when both were supplied.
type LiveInfo struct {
	FirstAudioMS    *float64 `json:"first_audio_ms"`
	PeakRSSMB       *float64 `json:"peak_rss_mb"`
	ResidentModelMB *float64 `json:"resident_model_mb"`
	MemoryReserveMB *float64 `json:"memory_reserve_mb"`
	ColdLoadMS      *float64 `json:"cold_load_ms"`
	WarmLoadMS      *float64 `json:"warm_load_ms"`
	// CacheEvictions and ConcurrentWorkloads are measured by the live benchmark (D-08);
	// this tool leaves them null.
	CacheEvictions      *int  `json:"cache_evictions"`
	ConcurrentWorkloads *bool `json:"concurrent_workloads"`
}

// Configuration is one of the five configurations on the corpus.
type Configuration struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	UsesRules       bool   `json:"uses_rules"`
	UsesScorer      bool   `json:"uses_scorer"`
	UsesController  bool   `json:"uses_controller"`
	ScorerCalls     int    `json:"scorer_calls"`
	ControllerCalls int    `json:"controller_calls"`

	Metrics     MetricSet        `json:"metrics"`
	ByCategory  []CategoryResult `json:"by_category"`
	ByLanguage  []LanguageResult `json:"by_language"`
	Calibration Calibration      `json:"calibration"`
	Latency     LatencyInfo      `json:"latency"`
	Cache       CacheInfo        `json:"cache"`
	Cases       []CaseOutcome    `json:"cases"`
}

// MetricSet is the metrics over a configuration's cases. Every metric is {passed, total,
// rate}; passed always counts the CORRECT behaviour, so higher is better. The false
// in-scope rate is 1 - out_of_scope_rejection, the false out-of-scope rate is
// 1 - in_scope_acceptance, the multilingual regression rate is 1 - pair_consistency.
type MetricSet struct {
	Cases                     int   `json:"cases"`
	TaskCompletion            Ratio `json:"task_completion"`
	CapabilitySelection       Ratio `json:"capability_selection"`
	CandidateSelection        Ratio `json:"candidate_selection"`
	ConfirmationAccuracy      Ratio `json:"confirmation_accuracy"`
	CancellationAccuracy      Ratio `json:"cancellation_accuracy"`
	CorrectionAccuracy        Ratio `json:"correction_accuracy"`
	InScopeAcceptance         Ratio `json:"in_scope_acceptance"`
	OutOfScopeRejection       Ratio `json:"out_of_scope_rejection"`
	UnauthorisedRejection     Ratio `json:"unauthorised_rejection"`
	InjectionResistance       Ratio `json:"injection_resistance"`
	UntrustedDocumentHandling Ratio `json:"untrusted_document_handling"`
	RAGOperation              Ratio `json:"rag_operation"`
	CitationCorrectness       Ratio `json:"citation_correctness"`
	FailClosed                Ratio `json:"fail_closed"`
	Clarification             Ratio `json:"clarification"`
	MultilingualQuality       Ratio `json:"multilingual_quality"`
	PairConsistency           Ratio `json:"pair_consistency"`
	ControllerCallsAvoided    Ratio `json:"controller_calls_avoided"`
	GeneratedToksAvoided      int   `json:"generated_toks_avoided"`
}

// CategoryResult is the task completion of one category.
type CategoryResult struct {
	Category string `json:"category"`
	Cases    int    `json:"cases"`
	Pass     Ratio  `json:"pass"`
}

// LanguageResult is the task completion of one language.
type LanguageResult struct {
	Language string `json:"language"`
	Cases    int    `json:"cases"`
	Pass     Ratio  `json:"pass"`
}

// LatencyInfo holds the latency percentiles with their sample counts (no means).
// DecisionMS is the time of the decision stage (rules and scorer); FirstResponseMS adds
// the controller call when one was made; FirstAudioMS adds the supplied first-chunk time
// of the voice pipeline and is nil unless it was supplied.
type LatencyInfo struct {
	DecisionMS      Stats  `json:"decision_ms"`
	FirstResponseMS Stats  `json:"first_response_ms"`
	FirstAudioMS    *Stats `json:"first_audio_ms"`
}

// CacheInfo is the warm replay of the requests the scorer accepted: the same requests
// sent again, of which Hits were answered from the engine's result cache.
type CacheInfo struct {
	Requests int `json:"requests"`
	Hits     int `json:"hits"`
}

// Decided-by values.
const (
	ByRules      = "rules"
	ByScorer     = "scorer"
	ByController = "controller"
	ByNone       = "none"
)

// Scorer statuses of a case.
const (
	ScorerAccepted  = "accepted"
	ScorerFallback  = "fallback"
	ScorerNotCalled = "not_called"
)

// CaseOutcome is the raw outcome of one case. The aggregate metrics are a pure function
// of a slice of these (Aggregate), which is how `check` verifies them.
type CaseOutcome struct {
	ID           string  `json:"id"`
	Pair         string  `json:"pair"`
	Language     string  `json:"language"`
	Category     string  `json:"category"`
	DecisionType string  `json:"decision_type"`
	DecidedBy    string  `json:"decided_by"`
	ExpectedKind string  `json:"expected_kind"`
	ExpectedID   *string `json:"expected_id"`
	OutcomeKind  string  `json:"outcome_kind"`
	SelectedID   *string `json:"selected_id"`
	Correct      bool    `json:"correct"`

	NeedsCitation  bool `json:"needs_citation"`
	UnsafeSelected bool `json:"unsafe_selected"`
	DocumentSent   bool `json:"document_sent"`

	ScorerCalled   bool     `json:"scorer_called"`
	ScorerStatus   string   `json:"scorer_status"`
	FallbackReason *string  `json:"fallback_reason"`
	Confidence     *float64 `json:"confidence"`
	ScorerCorrect  *bool    `json:"scorer_correct"`

	ControllerCalled      bool `json:"controller_called"`
	ControllerCallAvoided bool `json:"controller_call_avoided"`
	GeneratedToksAvoided  int  `json:"generated_toks_avoided"`

	DecisionMS      float64  `json:"decision_ms"`
	FirstResponseMS float64  `json:"first_response_ms"`
	FirstAudioMS    *float64 `json:"first_audio_ms"`
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
