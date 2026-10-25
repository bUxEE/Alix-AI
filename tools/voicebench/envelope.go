package main

import (
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
)

// The bench companion: a result in the exact envelope of schemas/bench/result.schema.json (the same keys, the same rules), so that
// `go run ./tools/bench check` validates it like any other result. The types, the aggregation and the statistics below are a copy of
// tools/rageval/envelope.go and stats.go (tools/bench is a main package and cannot be imported; companion_test.go runs the real
// `bench check` over a generated companion so drift between the copies cannot go unseen).
//
// Why a companion and not the result itself. The bench schema is closed (additionalProperties: false everywhere), its categories are the
// four of the TaskProposal benchmark and its metrics are the ones of that benchmark. A voice result has metrics it cannot hold (WER,
// real-time factor, time to first audio). The schema and tools/bench belong to M0-11 and are not changed here; the full result is the
// .voice file next to the companion (result.go), which `voicebench check` validates and cross-checks against it.
//
// What the companion carries is what fits without bending the meaning of a field: the NON-SPEECH clips (silence, noise, hum) of the
// protocol, which a correct voice pipeline must DECLINE (no transcript reaches the turn pipeline), as bench `refusal` cases. Their bench
// metrics then mean what their names say:
//
//	refusal_correctness   the pipeline (VAD, then STT) produced no transcript
//	intent                the outcome was no_transcript
//	catalogue_conformance nothing was produced that could enter the turn pipeline (same flag)
//	schema_valid          the pipeline returned a typed outcome (an error is not one)
//	pass                  the refusal rule
//
// capability_routing, field_* and injection_resistance do not apply (total 0). The speech clips are not in the companion: no bench field
// means word error rate, and relabelling one would make a green bench check that measures nothing it names.

// The types below are the Go form of schemas/bench/result.schema.json. Every
// key is always written (no omitempty) and a value that is unknown or does not
// apply is a nil pointer, which marshals as null.

// BenchFormatVersion is result.format_version.
const BenchFormatVersion = 1

// BenchResult is one result in the bench envelope.
type BenchResult struct {
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
	Corpus          BenchCorpus     `json:"corpus"`
	Config          BenchConfig     `json:"config"`
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

// BenchCorpus identifies the corpus of the companion.
type BenchCorpus struct {
	ID         string `json:"id"`
	SHA256     string `json:"sha256"`
	CasesTotal int    `json:"cases_total"`
	CasesRun   int    `json:"cases_run"`
}

// BenchConfig is how the run was configured.
type BenchConfig struct {
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

// MetricSet is the bench metrics over some set of cases.
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

// MemoryInfo is the container memory sampling block of the bench envelope: the
// "none" form unless the run was given a container to sample (--docker-container;
// dockermem.go), in which case it is the figure of 'docker stats'. The process
// memory of rageval itself is in the .rag result, not here.
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

func noMemory() MemoryInfo { return MemoryInfo{Source: MemSourceNone} }

// ErrorsInfo counts the cases that ended in an error.
type ErrorsInfo struct {
	Count  int            `json:"count"`
	ByCode map[string]int `json:"by_code"`
}

// Bench case categories and languages.
const (
	CatRouting    = "routing"
	CatExtraction = "extraction"
	CatRefusal    = "refusal"
	CatInjection  = "injection"
)

// CaseOutcome is the raw outcome of one case, in the bench envelope.
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

// ---------------------------------------------------------------- profiles

// profileSpec is what a hardware profile stands for: the caps in
// deploy/container/localai/compose.<profile>.yaml (master plan 7.1, ADR 0020).
type profileSpec struct {
	id    models.Profile
	memMB int
	cpus  float64
}

var profileSpecs = []profileSpec{
	{models.ProfileLite, 8192, 4},
	{models.ProfileBalanced, 16384, 8},
	{models.ProfileExperimental4G, 4096, 4},
}

// ParseProfile maps the command-line spelling to the registry's profile enum:
// lite, balanced and experimental-4g, with 4g as the short form of the last.
func ParseProfile(s string) (models.Profile, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "4g" {
		return models.ProfileExperimental4G, nil
	}
	for _, p := range profileSpecs {
		if string(p.id) == s {
			return p.id, nil
		}
	}
	return "", fmt.Errorf("unknown profile %q (use lite, balanced or 4g)", s)
}

// limitsFor returns the container limits the profile stands for.
func limitsFor(p models.Profile) (ContainerLimits, bool) {
	for _, s := range profileSpecs {
		if s.id == p {
			return ContainerLimits{MemMB: s.memMB, CPUs: s.cpus, Source: "profile-definition"}, true
		}
	}
	return ContainerLimits{}, false
}

// isEmulated reports whether results under profile are emulated. Every profile
// the tool knows is a resource-limited one (cgroup caps on a machine that is
// larger than the profile), so what it measures is evidence for
// qualified-emulated, never for certified (master plan 7.4). There is no way to
// write emulated: false.
func isEmulated(p models.Profile) bool {
	_, known := limitsFor(p)
	return known
}

// ---------------------------------------------------------------- aggregation

// Aggregate computes the bench metrics over outcomes. It reads only the flags
// the judge set, so the same function re-derives the aggregates of a stored
// result from its cases (it is a copy of tools/bench's, with the same
// denominators; see the package comment of tools/bench/scoring.go).
func Aggregate(cases []CaseOutcome) MetricSet {
	var m MetricSet
	var (
		valid, intent, catalogue, pass           int
		capOK, capTotal                          int
		fieldOK, fieldTotal, valueHits, valueN   int
		refusalOK, refusalTotal, resOK, resTotal int
	)
	for _, c := range cases {
		m.Cases++
		if c.SchemaValid {
			valid++
		}
		if c.IntentOK {
			intent++
		}
		if c.CatalogueOK {
			catalogue++
		}
		if c.Pass {
			pass++
		}
		if c.CapabilityOK != nil {
			capTotal++
			if *c.CapabilityOK {
				capOK++
			}
		}
		if c.FieldsExact != nil {
			fieldTotal++
			if *c.FieldsExact {
				fieldOK++
			}
			valueN += c.FieldTotal
			valueHits += c.FieldHits
		}
		if c.RefusalOK != nil {
			refusalTotal++
			if *c.RefusalOK {
				refusalOK++
			}
		}
		if c.Resisted != nil {
			resTotal++
			if *c.Resisted {
				resOK++
			}
		}
	}
	m.SchemaValid = newRatio(valid, m.Cases)
	m.Intent = newRatio(intent, m.Cases)
	m.CatalogueConformity = newRatio(catalogue, m.Cases)
	m.Pass = newRatio(pass, m.Cases)
	m.CapabilityRouting = newRatio(capOK, capTotal)
	m.FieldExactMatch = newRatio(fieldOK, fieldTotal)
	m.FieldValueAccuracy = newRatio(valueHits, valueN)
	m.RefusalCorrectness = newRatio(refusalOK, refusalTotal)
	m.InjectionResistance = newRatio(resOK, resTotal)
	return m
}

func filterCases(cases []CaseOutcome, keep func(CaseOutcome) bool) []CaseOutcome {
	var out []CaseOutcome
	for _, c := range cases {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// aggregateBy builds the per-category and per-language blocks.
func aggregateBy(cases []CaseOutcome) (ByCategory, ByLanguage) {
	cat := func(name string) MetricSet {
		return Aggregate(filterCases(cases, func(c CaseOutcome) bool { return c.Category == name }))
	}
	lang := func(name string) MetricSet {
		return Aggregate(filterCases(cases, func(c CaseOutcome) bool { return c.Language == name }))
	}
	return ByCategory{
			Routing: cat(CatRouting), Extraction: cat(CatExtraction), Refusal: cat(CatRefusal), Injection: cat(CatInjection),
		},
		ByLanguage{EN: lang("en"), IT: lang("it")}
}

// summarizeLatency derives the latency block from the outcomes alone, which is
// how `bench check` verifies it.
func summarizeLatency(cases []CaseOutcome, warmupFirst *float64) LatencyInfo {
	var totals, ttfts, prompts []float64
	var toksIn, toksOut, decodeToks int
	var totalMS, decodeMS float64
	for _, c := range cases {
		if c.Status != StatusOK || c.LatencyMS == nil {
			continue
		}
		totals = append(totals, *c.LatencyMS)
		prompts = append(prompts, float64(c.PromptToksEst))
		totalMS += *c.LatencyMS
		out := 0
		if c.ToksOut != nil {
			out = *c.ToksOut
			toksOut += out
		}
		if c.ToksIn != nil {
			toksIn += *c.ToksIn
		}
		if c.TTFTMS != nil {
			ttfts = append(ttfts, *c.TTFTMS)
			if gen := *c.LatencyMS - *c.TTFTMS; gen > 0 {
				decodeMS += gen
				decodeToks += out
			}
		}
	}
	info := LatencyInfo{
		TotalMS: Summarize(totals), TTFTMS: Summarize(ttfts), PromptToksEst: Summarize(prompts),
		ToksIn: toksIn, ToksOut: toksOut, WarmupFirstRequestMS: warmupFirst,
	}
	if totalMS > 0 {
		info.ToksPerSecond = round3(float64(toksOut) / (totalMS / 1000))
	}
	if decodeMS > 0 {
		info.DecodeToksPerSecond = round3(float64(decodeToks) / (decodeMS / 1000))
	}
	return info
}

// summarizeErrors counts the cases that ended in an error by code.
func summarizeErrors(cases []CaseOutcome) ErrorsInfo {
	e := ErrorsInfo{ByCode: map[string]int{}}
	for _, c := range cases {
		if c.Status == StatusError {
			e.Count++
			if c.ErrorCode != nil {
				e.ByCode[*c.ErrorCode]++
			}
		}
	}
	return e
}
